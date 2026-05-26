package types

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// DatabaseService 数据库服务
type DatabaseService struct {
	db  *gorm.DB
	cfg *Config
}

// NewDatabaseService 创建新的数据库服务
func NewDatabaseService(cfg *Config) (*DatabaseService, error) {
	db, err := gorm.Open(sqlite.Open(cfg.SqlitePath), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// 自动迁移数据库结构
	db.AutoMigrate(&SubmitCtx{})
	db.AutoMigrate(&User{})

	// queued submissions are recovered by the scheduler after startup. Any
	// other non-terminal status belonged to a process that was interrupted.
	db.Model(&SubmitCtx{}).
		Where("status NOT IN ?", []string{"completed", "dead", "failed", "queued"}).
		Updates(map[string]interface{}{"status": "dead", "msg": "judge process stopped before completion"})

	return &DatabaseService{
		db:  db,
		cfg: cfg,
	}, nil
}

// GetDB 获取数据库实例
func (ds *DatabaseService) GetDB() *gorm.DB {
	return ds.db
}

// ===============================
// 用户操作
// ===============================

// CreateUser 创建新用户
func (ds *DatabaseService) CreateUser(userID string) (*User, error) {
	user := &User{
		ID:             userID,
		Token:          uuid.New().String(),
		BestScores:     make(map[string]float64),
		BestSubmits:    make(map[string]string),
		BestSubmitDate: make(map[string]int64),
		BestTags:       make(map[string]string),
		TotalScore:     0,
	}

	result := ds.db.Create(user)
	if result.Error != nil {
		return nil, result.Error
	}

	log.Info().Str("user", userID).Msg("Created new user")
	return user, nil
}

// applyRankUpdate writes the user's best record for a problem according to
// the problem's RankUpdate rule (see types.Problem.RankUpdate). Failed or
// not-yet-completed submits never write. The caller must have called
// ensureMaps(u) first.
func applyRankUpdate(u *User, problem *Problem, s *SubmitCtx) {
	if s.Invalid {
		return
	}
	if s.Status != "completed" || !s.JudgeResult.Success {
		return
	}
	problemID := s.Problem
	newScore := s.JudgeResult.Score * problem.Weight

	var shouldUpdate bool
	switch strings.ToLower(strings.TrimSpace(problem.RankUpdate)) {
	case "always", "latest":
		// 用 BestSubmitDate 比较而不是无脑覆盖，避免 DoFullUserScan 时加载顺序
		// 影响最终结果。等号让同一次提交多次扫描时也能稳定写入（幂等）。
		shouldUpdate = u.BestSubmitDate[problemID] <= s.SubmitTime
	default: // "", "best", 或任何无法识别的值
		oldScore, hasOldScore := u.BestScores[problemID]
		shouldUpdate = !hasOldScore || oldScore < newScore ||
			(oldScore == newScore && speedupTagGreater(s.JudgeResult.Tag, u.BestTags[problemID]))
	}
	if !shouldUpdate {
		return
	}
	u.BestScores[problemID] = newScore
	u.BestSubmits[problemID] = s.ID
	u.BestSubmitDate[problemID] = s.SubmitTime
	u.BestTags[problemID] = s.JudgeResult.Tag
}

// ensureMaps lazy-initializes all JMap fields on User so old rows (where the
// column was NULL because the field didn't exist at insert time) don't panic
// on assignment. Safe to call on a freshly created user too.
func ensureMaps(u *User) {
	if u.BestScores == nil {
		u.BestScores = make(JMapStrFloat64)
	}
	if u.BestSubmits == nil {
		u.BestSubmits = make(JMapStrString)
	}
	if u.BestSubmitDate == nil {
		u.BestSubmitDate = make(JMapStrInt64)
	}
	if u.BestTags == nil {
		u.BestTags = make(JMapStrString)
	}
}

// GetUserByID 根据ID获取用户
func (ds *DatabaseService) GetUserByID(userID string) (*User, error) {
	var user User
	result := ds.db.Where("id = ?", userID).First(&user)
	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			// 用户不存在，创建新用户
			return ds.CreateUser(userID)
		}
		return nil, result.Error
	}
	ensureMaps(&user)
	return &user, nil
}

// GetUserByToken 根据Token获取用户
func (ds *DatabaseService) GetUserByToken(token string) (*User, error) {
	var user User
	result := ds.db.Where("token = ?", token).First(&user)
	if result.Error != nil {
		return nil, result.Error
	}
	ensureMaps(&user)
	return &user, nil
}

// UpdateUser 更新用户信息
func (ds *DatabaseService) UpdateUser(user *User) error {
	user.CalculateTotalScore()
	result := ds.db.Save(user)
	return result.Error
}

// GetAllUsersOrderedByScore 获取按分数排序的所有用户
func (ds *DatabaseService) GetAllUsersOrderedByScore() ([]User, error) {
	var users []User
	result := ds.db.Order("total_score desc").Find(&users)
	sort.SliceStable(users, func(i, j int) bool {
		if users[i].TotalScore != users[j].TotalScore {
			return users[i].TotalScore > users[j].TotalScore
		}
		return userSpeedupScore(users[i]) > userSpeedupScore(users[j])
	})
	return users, result.Error
}

func parseSpeedupTag(tag string) (float64, bool) {
	tag = strings.TrimSpace(tag)
	if len(tag) < 2 || !strings.EqualFold(tag[len(tag)-1:], "x") {
		return 0, false
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(tag[:len(tag)-1]), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func speedupTagGreater(candidate, current string) bool {
	candidateValue, candidateOK := parseSpeedupTag(candidate)
	if !candidateOK {
		return false
	}
	currentValue, currentOK := parseSpeedupTag(current)
	return !currentOK || candidateValue > currentValue
}

func userSpeedupScore(user User) float64 {
	var score float64
	for _, tag := range user.BestTags {
		if value, ok := parseSpeedupTag(tag); ok {
			score += value
		}
	}
	return score
}

// UpdateUserSubmitResult 更新用户提交结果
func (ds *DatabaseService) UpdateUserSubmitResult(userID string, submit *SubmitCtx, problem *Problem) error {
	user, err := ds.GetUserByID(userID)
	if err != nil {
		return err
	}
	ensureMaps(user)
	applyRankUpdate(user, problem, submit)
	return ds.UpdateUser(user)
}

// DoFullUserScan 全量用户扫描和重计算
func (ds *DatabaseService) DoFullUserScan(problems map[string]Problem) error {
	var submits []SubmitCtx
	ds.db.Find(&submits)

	var users []User
	ds.db.Find(&users)

	userMap := make(map[string]User)
	for _, user := range users {
		ensureMaps(&user)
		// 清掉当前 problems 集合里每条 problem 的 Best* 条目，让扫描从零重建。
		// 这样 Invalid 化某条 submit 后，best 规则能正确回退到次佳，
		// always 规则在全部失效时能正确清空。不动 problems 之外的条目，
		// 避免误伤已删除题目残留的历史排名。
		for problemID := range problems {
			delete(user.BestScores, problemID)
			delete(user.BestSubmits, problemID)
			delete(user.BestSubmitDate, problemID)
			delete(user.BestTags, problemID)
		}
		userMap[user.ID] = user
	}

	for _, s := range submits {
		u, ok := userMap[s.User]
		if !ok {
			log.Fatal().Msg("Encountered corrupted data, submitted user does not exist in User table")
		}

		if problem, exists := problems[s.Problem]; exists {
			applyRankUpdate(&u, &problem, &s)
		}

		userMap[s.User] = u
	}

	for _, u := range userMap {
		u.CalculateTotalScore()
		ds.db.Save(&u)
	}

	return nil
}

// IsAdmin 检查用户是否为管理员
func (ds *DatabaseService) IsAdmin(userID string) bool {
	for _, admin := range ds.cfg.Admins {
		if admin == userID {
			return true
		}
	}
	return false
}

// ===============================
// 提交记录操作
// ===============================

// CreateSubmit 创建新提交
func (ds *DatabaseService) CreateSubmit(submit *SubmitCtx) error {
	submit.LastUpdate = time.Now().UnixNano()
	result := ds.db.Create(submit)
	return result.Error
}

// UpdateSubmit 更新提交记录
func (ds *DatabaseService) UpdateSubmit(submit *SubmitCtx) error {
	submit.LastUpdate = time.Now().UnixNano()
	result := ds.db.Save(submit)
	return result.Error
}

// GetSubmitByID 根据ID获取提交记录
func (ds *DatabaseService) GetSubmitByID(submitID string) (*SubmitCtx, error) {
	var submit SubmitCtx
	result := ds.db.Where("id = ?", submitID).First(&submit)
	if result.Error != nil {
		return nil, result.Error
	}
	return &submit, nil
}

// GetQueuedSubmits returns queued submissions in FIFO order for scheduler recovery.
func (ds *DatabaseService) GetQueuedSubmits() ([]SubmitCtx, error) {
	var submits []SubmitCtx
	result := ds.db.Where("status = ?", "queued").
		Order("submit_time asc").
		Find(&submits)
	return submits, result.Error
}

// CountSubmitsAhead returns queued submissions older than submitTime plus any
// currently-active judge status.
func (ds *DatabaseService) CountSubmitsAhead(submitTime int64) (int64, error) {
	var queued int64
	if err := ds.db.Model(&SubmitCtx{}).
		Where("status = ? AND submit_time < ?", "queued", submitTime).
		Count(&queued).Error; err != nil {
		return 0, err
	}

	var active int64
	activeStatuses := []string{"running", "prep_dirs", "prep_files", "run_workflow", "collect_result"}
	if err := ds.db.Model(&SubmitCtx{}).
		Where("status IN ? OR status LIKE ?", activeStatuses, "run_workflow-%").
		Count(&active).Error; err != nil {
		return 0, err
	}

	return queued + active, nil
}

// GetSubmitsByUser 获取用户的提交记录（分页）
func (ds *DatabaseService) GetSubmitsByUser(userID string, page, limit int) ([]SubmitCtx, int64, error) {
	var submits []SubmitCtx
	var total int64

	// 获取总数
	ds.db.Model(&SubmitCtx{}).Where("user = ?", userID).Count(&total)

	// 获取分页数据
	result := ds.db.Where("user = ?", userID).
		Order("submit_time desc").
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&submits)

	return submits, total, result.Error
}

// GetAllSubmits 获取所有提交记录（分页）
func (ds *DatabaseService) GetAllSubmits(page, limit int) ([]SubmitCtx, int64, error) {
	var submits []SubmitCtx
	var total int64

	// 获取总数
	ds.db.Model(&SubmitCtx{}).Count(&total)

	// 获取分页数据
	result := ds.db.Order("submit_time desc").
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&submits)

	return submits, total, result.Error
}

// GetSubmitsForAPI 获取API用的提交记录（分页，只包含基本信息）
func (ds *DatabaseService) GetSubmitsForAPI(page, limit int) ([]SubmitCtx, int64, error) {
	var submits []SubmitCtx
	var total int64

	// 获取总数
	ds.db.Model(&SubmitCtx{}).Count(&total)

	// 获取分页数据，只选择需要的字段
	result := ds.db.Select("id", "user", "problem", "submit_time", "status", "msg", "judge_result", "invalid").
		Order("submit_time desc").
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&submits)

	return submits, total, result.Error
}

// FindSubmitsByUserAndPattern 根据用户和模式查找提交（用于模糊搜索）
func (ds *DatabaseService) FindSubmitsByUserAndPattern(userID, pattern string) (*SubmitCtx, error) {
	var submit SubmitCtx
	result := ds.db.Order("submit_time desc").
		Where("id LIKE ? AND user = ?", "%"+pattern+"%", userID).
		First(&submit)
	if result.Error != nil {
		return nil, result.Error
	}
	return &submit, nil
}

// GetSubmitCount 获取提交总数
func (ds *DatabaseService) GetSubmitCount() (int64, error) {
	var count int64
	result := ds.db.Model(&SubmitCtx{}).Count(&count)
	return count, result.Error
}

// GetUserSubmitCount 获取用户提交总数
func (ds *DatabaseService) GetUserSubmitCount(userID string) (int64, error) {
	var count int64
	result := ds.db.Model(&SubmitCtx{}).Where("user = ?", userID).Count(&count)
	return count, result.Error
}

// DeleteOldSubmits 删除旧的提交记录（可选功能）
func (ds *DatabaseService) DeleteOldSubmits(beforeTime time.Time) error {
	result := ds.db.Where("submit_time < ?", beforeTime.UnixNano()).Delete(&SubmitCtx{})
	return result.Error
}

// GetSubmitStatistics 获取提交统计信息
func (ds *DatabaseService) GetSubmitStatistics() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// 总提交数
	var totalSubmits int64
	ds.db.Model(&SubmitCtx{}).Count(&totalSubmits)
	stats["total_submits"] = totalSubmits

	// 成功提交数
	var successSubmits int64
	ds.db.Model(&SubmitCtx{}).Where("status = ?", "completed").Count(&successSubmits)
	stats["success_submits"] = successSubmits

	// 失败提交数
	var failedSubmits int64
	ds.db.Model(&SubmitCtx{}).Where("status = ?", "failed").Count(&failedSubmits)
	stats["failed_submits"] = failedSubmits

	// 总用户数
	var totalUsers int64
	ds.db.Model(&User{}).Count(&totalUsers)
	stats["total_users"] = totalUsers

	return stats, nil
}

// MarkSubmitInvalid sets the Invalid flag on a single submit record. Returns
// the updated record so callers can locate (user, problem) for a follow-up
// RecomputeUserProblemBest.
func (ds *DatabaseService) MarkSubmitInvalid(submitID string, invalid bool) (*SubmitCtx, error) {
	submit, err := ds.GetSubmitByID(submitID)
	if err != nil {
		return nil, err
	}
	submit.Invalid = invalid
	submit.LastUpdate = time.Now().UnixNano()
	if err := ds.db.Save(submit).Error; err != nil {
		return nil, err
	}
	return submit, nil
}

// MarkSubmitsInvalidByUserProblem flips Invalid=true on every non-invalid
// submit for the given (user, problem). Returns rows affected. Used by Rejudge
// before re-evaluating: previous submissions for that (user, problem) become
// historical-only and stop participating in rank calculations.
func (ds *DatabaseService) MarkSubmitsInvalidByUserProblem(userID, problemID string) (int64, error) {
	result := ds.db.Model(&SubmitCtx{}).
		Where("user = ? AND problem = ? AND invalid = ?", userID, problemID, false).
		Updates(map[string]interface{}{
			"invalid":     true,
			"last_update": time.Now().UnixNano(),
		})
	return result.RowsAffected, result.Error
}

// RecomputeUserProblemBest rebuilds the user's Best* entry for one problem
// by scanning all of their submits for that problem and replaying
// applyRankUpdate. applyRankUpdate skips Invalid submits, so the resulting
// Best* reflects only currently-valid submissions — picking up "second best"
// under the best rule and clearing the entry when no valid passing submit
// remains.
func (ds *DatabaseService) RecomputeUserProblemBest(userID, problemID string, problem *Problem) error {
	user, err := ds.GetUserByID(userID)
	if err != nil {
		return err
	}
	ensureMaps(user)
	delete(user.BestScores, problemID)
	delete(user.BestSubmits, problemID)
	delete(user.BestSubmitDate, problemID)
	delete(user.BestTags, problemID)

	var submits []SubmitCtx
	if err := ds.db.Where("user = ? AND problem = ?", userID, problemID).Find(&submits).Error; err != nil {
		return err
	}
	for i := range submits {
		applyRankUpdate(user, problem, &submits[i])
	}
	return ds.UpdateUser(user)
}
