package types

import (
	"path/filepath"
	"testing"
)

func newTestDatabaseService(t *testing.T) *DatabaseService {
	t.Helper()

	db, err := NewDatabaseService(&Config{
		SqlitePath: filepath.Join(t.TempDir(), "soj.db"),
	})
	if err != nil {
		t.Fatalf("NewDatabaseService: %v", err)
	}
	return db
}

func TestParseSpeedupTag(t *testing.T) {
	tests := []struct {
		name   string
		tag    string
		want   float64
		wantOK bool
	}{
		{name: "decimal", tag: "123.5x", want: 123.5, wantOK: true},
		{name: "spaces", tag: " 6.00x ", want: 6, wantOK: true},
		{name: "uppercase suffix", tag: "8X", want: 8, wantOK: true},
		{name: "missing suffix", tag: "8", wantOK: false},
		{name: "invalid number", tag: "fastx", wantOK: false},
		{name: "nan", tag: "NaNx", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseSpeedupTag(tt.tag)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("value=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyRankUpdateUsesSpeedupTagAsBestTieBreaker(t *testing.T) {
	user := User{
		ID:             "alice",
		BestScores:     make(JMapStrFloat64),
		BestSubmits:    make(JMapStrString),
		BestSubmitDate: make(JMapStrInt64),
		BestTags:       make(JMapStrString),
	}
	problem := Problem{Id: "p1", Weight: 1}

	applyRankUpdate(&user, &problem, &SubmitCtx{
		ID:         "slow",
		Problem:    "p1",
		Status:     "completed",
		SubmitTime: 1,
		JudgeResult: JudgeResult{
			Success: true,
			Score:   100,
			Tag:     "10x",
		},
	})
	applyRankUpdate(&user, &problem, &SubmitCtx{
		ID:         "fast",
		Problem:    "p1",
		Status:     "completed",
		SubmitTime: 2,
		JudgeResult: JudgeResult{
			Success: true,
			Score:   100,
			Tag:     "123.5x",
		},
	})
	applyRankUpdate(&user, &problem, &SubmitCtx{
		ID:         "slower",
		Problem:    "p1",
		Status:     "completed",
		SubmitTime: 3,
		JudgeResult: JudgeResult{
			Success: true,
			Score:   100,
			Tag:     "20x",
		},
	})

	if user.BestSubmits["p1"] != "fast" {
		t.Fatalf("best submit=%q, want fast", user.BestSubmits["p1"])
	}
	if user.BestTags["p1"] != "123.5x" {
		t.Fatalf("best tag=%q, want 123.5x", user.BestTags["p1"])
	}
}

func TestApplyRankUpdateSkipsInvalid(t *testing.T) {
	user := User{
		ID:             "alice",
		BestScores:     make(JMapStrFloat64),
		BestSubmits:    make(JMapStrString),
		BestSubmitDate: make(JMapStrInt64),
		BestTags:       make(JMapStrString),
	}
	problem := Problem{Id: "p1", Weight: 1}

	applyRankUpdate(&user, &problem, &SubmitCtx{
		ID:         "s1",
		Problem:    "p1",
		Status:     "completed",
		SubmitTime: 1,
		Invalid:    true,
		JudgeResult: JudgeResult{Success: true, Score: 100},
	})

	if _, ok := user.BestScores["p1"]; ok {
		t.Fatalf("Invalid submit must not produce a Best* entry, got %v", user.BestScores)
	}
}

func TestRecomputeUserProblemBestFallsBackToSecondBest(t *testing.T) {
	ds := newTestDatabaseService(t)
	user, err := ds.CreateUser("alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pb := Problem{Id: "p1", Weight: 1}

	high := SubmitCtx{
		ID:          "high",
		User:        user.ID,
		Problem:     "p1",
		Status:      "completed",
		SubmitTime:  100,
		JudgeResult: JudgeResult{Success: true, Score: 80},
	}
	low := SubmitCtx{
		ID:          "low",
		User:        user.ID,
		Problem:     "p1",
		Status:      "completed",
		SubmitTime:  200,
		JudgeResult: JudgeResult{Success: true, Score: 60},
	}
	if err := ds.db.Create(&high).Error; err != nil {
		t.Fatalf("create high: %v", err)
	}
	if err := ds.db.Create(&low).Error; err != nil {
		t.Fatalf("create low: %v", err)
	}

	// Initial recompute should pick "high" (rankupdate=best default, 80 > 60).
	if err := ds.RecomputeUserProblemBest(user.ID, "p1", &pb); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	u, _ := ds.GetUserByID(user.ID)
	if u.BestSubmits["p1"] != "high" || u.BestScores["p1"] != 80 {
		t.Fatalf("expected best=high/80, got submit=%q score=%v", u.BestSubmits["p1"], u.BestScores["p1"])
	}

	// Invalidate "high"; recompute should fall back to "low".
	if _, err := ds.MarkSubmitInvalid("high", true); err != nil {
		t.Fatalf("mark invalid: %v", err)
	}
	if err := ds.RecomputeUserProblemBest(user.ID, "p1", &pb); err != nil {
		t.Fatalf("recompute after invalidate: %v", err)
	}
	u, _ = ds.GetUserByID(user.ID)
	if u.BestSubmits["p1"] != "low" || u.BestScores["p1"] != 60 {
		t.Fatalf("expected best=low/60 after invalidating high, got submit=%q score=%v", u.BestSubmits["p1"], u.BestScores["p1"])
	}

	// Invalidate "low" too; recompute should clear the entry.
	if _, err := ds.MarkSubmitInvalid("low", true); err != nil {
		t.Fatalf("mark invalid low: %v", err)
	}
	if err := ds.RecomputeUserProblemBest(user.ID, "p1", &pb); err != nil {
		t.Fatalf("recompute after invalidate all: %v", err)
	}
	u, _ = ds.GetUserByID(user.ID)
	if _, ok := u.BestScores["p1"]; ok {
		t.Fatalf("expected Best* cleared after all invalid, got %v", u.BestScores)
	}
	if u.TotalScore != 0 {
		t.Fatalf("expected total=0, got %v", u.TotalScore)
	}
}

func TestGetAllUsersOrderedByScoreUsesSpeedupTagTieBreaker(t *testing.T) {
	db := newTestDatabaseService(t)
	users := []User{
		{ID: "slow", BestTags: JMapStrString{"p1": "10x"}, TotalScore: 100},
		{ID: "plain", BestTags: JMapStrString{"p1": "not-speedup"}, TotalScore: 100},
		{ID: "fast", BestTags: JMapStrString{"p1": "123.5x"}, TotalScore: 100},
		{ID: "higher-score", BestTags: JMapStrString{"p1": "1x"}, TotalScore: 101},
	}
	for i := range users {
		users[i].Token = users[i].ID + "-token"
		users[i].BestScores = make(JMapStrFloat64)
		users[i].BestSubmits = make(JMapStrString)
		users[i].BestSubmitDate = make(JMapStrInt64)
		if err := db.db.Create(&users[i]).Error; err != nil {
			t.Fatalf("create user %s: %v", users[i].ID, err)
		}
	}

	got, err := db.GetAllUsersOrderedByScore()
	if err != nil {
		t.Fatalf("GetAllUsersOrderedByScore: %v", err)
	}

	wantIDs := []string{"higher-score", "fast", "slow", "plain"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("user[%d]=%q, want %q", i, got[i].ID, want)
		}
	}
}
