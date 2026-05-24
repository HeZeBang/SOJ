package judge

import (
	"bytes"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mrhaoxx/SOJ/types"
)

type blockingRunner struct {
	db      *types.DatabaseService
	started chan string
	release chan struct{}
}

func newBlockingRunner(db *types.DatabaseService) *blockingRunner {
	return &blockingRunner{
		db:      db,
		started: make(chan string, 8),
		release: make(chan struct{}, 8),
	}
}

func (r *blockingRunner) RunJudge(ctx *types.SubmitCtx, problem *types.Problem) {
	r.started <- ctx.ID
	<-r.release
	ctx.SetStatus("completed")
	ctx.JudgeResult.Success = true
	ctx.JudgeResult.Score = 100
	_ = r.db.UpdateSubmit(ctx)
	close(ctx.Running)
}

func newTestScheduler(t *testing.T) (*types.Config, *types.DatabaseService, *blockingRunner, *Scheduler, types.Problem) {
	t.Helper()

	dir := t.TempDir()
	cfg := &types.Config{
		SqlitePath:        filepath.Join(dir, "soj.db"),
		SubmitsDir:        filepath.Join(dir, "submits"),
		SubmitWorkDir:     filepath.Join(dir, "work"),
		RealSubmitWorkDir: filepath.Join(dir, "real-work"),
	}
	db, err := types.NewDatabaseService(cfg)
	if err != nil {
		t.Fatalf("NewDatabaseService: %v", err)
	}
	if _, err := db.GetUserByID("alice"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	problem := types.Problem{Id: "p1", Weight: 1}
	runner := newBlockingRunner(db)
	scheduler := NewScheduler(cfg, db, runner, map[string]types.Problem{problem.Id: problem})
	return cfg, db, runner, scheduler, problem
}

func newSubmit(id string, submitTime int64) types.SubmitCtx {
	return types.SubmitCtx{
		ID:         id,
		User:       "alice",
		Problem:    "p1",
		SubmitTime: submitTime,
		Status:     "queued",
		Msg:        "waiting",
		Userface: types.Userface{
			Buffer: bytes.NewBuffer(nil),
		},
		Running: make(chan struct{}),
	}
}

func waitStarted(t *testing.T, runner *blockingRunner, want string) {
	t.Helper()
	select {
	case got := <-runner.started:
		if got != want {
			t.Fatalf("started %s, want %s", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s to start", want)
	}
}

func assertNotStarted(t *testing.T, runner *blockingRunner) {
	t.Helper()
	select {
	case got := <-runner.started:
		t.Fatalf("unexpectedly started %s", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSchedulerRunsSubmissionsExclusively(t *testing.T) {
	_, db, runner, scheduler, problem := newTestScheduler(t)

	first := newSubmit("1", 1)
	second := newSubmit("2", 2)
	if err := db.CreateSubmit(&first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := db.CreateSubmit(&second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	if ahead, err := scheduler.Enqueue(&first, problem); err != nil || ahead != 0 {
		t.Fatalf("enqueue first ahead=%d err=%v", ahead, err)
	}
	if ahead, err := scheduler.Enqueue(&second, problem); err != nil || ahead != 1 {
		t.Fatalf("enqueue second ahead=%d err=%v", ahead, err)
	}

	waitStarted(t, runner, "1")
	assertNotStarted(t, runner)

	runner.release <- struct{}{}
	waitStarted(t, runner, "2")
	runner.release <- struct{}{}
}

func TestSchedulerRecoversQueuedSubmissions(t *testing.T) {
	_, db, runner, scheduler, _ := newTestScheduler(t)

	submit := newSubmit(strconv.FormatInt(time.Now().UnixNano(), 10), time.Now().UnixNano())
	if err := db.CreateSubmit(&submit); err != nil {
		t.Fatalf("create submit: %v", err)
	}
	if err := scheduler.RecoverQueued(); err != nil {
		t.Fatalf("recover queued: %v", err)
	}

	waitStarted(t, runner, submit.ID)
	runner.release <- struct{}{}
}
