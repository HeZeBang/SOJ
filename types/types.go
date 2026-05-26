package types

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/logrusorgru/aurora/v4"
)

// AuthConfig SSH 认证配置
type AuthConfig struct {
	Mode             string   `yaml:"Mode"`             // "single", "github-list". 空则自动推断
	AllowedSSHPubkey string   `yaml:"AllowedSSHPubkey"` // Mode=single 时使用
	GitHubUsers      []string `yaml:"GitHubUsers"`      // Mode=github-list 时使用
	GitHubToken      string   `yaml:"GitHubToken"`      // 可选，避免 rate limit
	GitHubEndpoint   string   `yaml:"GitHubEndpoint"`   // 可选，默认 https://github.com，支持 GitHub Enterprise
	KeyCachePath     string   `yaml:"KeyCachePath"`     // 可选，密钥缓存文件路径（默认 ./keys_cache.json）
}

// Config 全局配置
type Config struct {
	HostKey    string `yaml:"HostKey"`
	ListenAddr string `yaml:"ListenAddr"`
	APIAddr    string `yaml:"APIAddr"`

	AllowedSSHPubkey string `yaml:"AllowedSSHPubkey"` // 向后兼容，等价于 Auth.Mode=single + Auth.AllowedSSHPubkey

	Auth AuthConfig `yaml:"Auth"`

	SubmitsDir    string `yaml:"SubmitsDir"`
	SubmitWorkDir string `yaml:"SubmitWorkDir"`
	ProblemsDir   string `yaml:"ProblemsDir"`

	// Used by `soj import` to stage built images and scaffold directories.
	ImagesDir   string `yaml:"ImagesDir"`
	ScaffoldDir string `yaml:"ScaffoldDir"`

	RealSubmitsDir    string `yaml:"RealSubmitsDir"`
	RealSubmitWorkDir string `yaml:"RealSubmitWorkDir"`

	SqlitePath string `yaml:"SqlitePath"`

	DockerCli        string `yaml:"DockerCli"`
	ProblemURLPrefix string `yaml:"ProblemURLPrefix"`
	SftpImage        string `yaml:"SftpImage"`

	DefaultMaskFiles []string `yaml:"DefaultMaskFiles"`
	DefaultMaskDirs  []string `yaml:"DefaultMaskDirs"`

	DefaultProperties []string `yaml:"DefaultProperties"`

	// 默认安全配置。每个 workflow 可以覆盖（或追加 AddCaps）。
	DefaultNoPrivs  bool     `yaml:"DefaultNoPrivs"`
	DefaultDropCaps []string `yaml:"DefaultDropCaps"`
	DefaultAddCaps  []string `yaml:"DefaultAddCaps"`
	DefaultSeccomp  string   `yaml:"DefaultSeccomp"`

	SubmitGid int `yaml:"SubmitGid"`
	SubmitUid int `yaml:"SubmitUid"`

	Admins []string `yaml:"Admins"`
}

// JudgeResult 评测结果
type JudgeResult struct {
	Success bool    `json:"success"`
	Score   float64 `json:"score"`
	Msg     string  `json:"message"`
	Memory  uint64  `json:"memory"` // in bytes
	Time    uint64  `json:"time"`   // in ns
	Tag     string  `json:"tag"`    // optional tag (e.g. "6.00x")
}

// WorkflowResult 工作流结果
type WorkflowResult struct {
	Success  bool                 `json:"success"`
	Logs     string               `json:"logs"`
	ExitCode int                  `json:"exit_code"`
	Steps    []WorkflowStepResult `json:"steps"`
}

// WorkflowStepResult 工作流步骤结果
type WorkflowStepResult struct {
	Logs     string `json:"logs"`
	ExitCode int    `json:"exit_code"`
}

// Userface 用户界面包装器
type Userface struct {
	*bytes.Buffer
	io.Writer
}

func (f Userface) Println(a ...interface{}) (n int, err error) {
	return fmt.Fprintln(f, a...)
}

func (f Userface) Printf(format string, a ...interface{}) (n int, err error) {
	return fmt.Fprintf(f, format, a...)
}

func (f Userface) Write(p []byte) (n int, err error) {
	var _f io.Writer
	if f.Writer != nil {
		_f = io.MultiWriter(f.Buffer, f.Writer)
	} else {
		_f = f.Buffer
	}
	_f.Write(p)
	return len(p), nil
}

// SubmitHash 提交文件哈希
type SubmitHash struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// SubmitCtx 提交上下文
type SubmitCtx struct {
	ID      string `gorm:"primaryKey" json:"id"`
	User    string `json:"user"`
	Problem string `json:"problem"`

	SubmitTime int64 `json:"submit_time"`
	LastUpdate int64 `json:"last_update"`

	Status string `json:"status"`
	Msg    string `json:"message"`

	Invalid bool `json:"invalid"`

	SubmitDir       string          `gorm:"-" json:"-"`
	SubmitsHashes   SubmitsHashes   `json:"submits_hashes"`
	Workdir         string          `gorm:"-" json:"-"`
	WorkflowResults WorkflowResults `json:"workflow_results"`
	JudgeResult     JudgeResult     `json:"judge_result"`

	RealWorkdir string `gorm:"-" json:"-"`

	Running  chan struct{} `gorm:"-" json:"-"`
	Userface Userface      `gorm:"-" json:"-"`
}

func (ctx *SubmitCtx) SetStatus(status string) *SubmitCtx {
	ctx.Status = status
	ctx.LastUpdate = time.Now().UnixNano()
	return ctx
}

func (ctx *SubmitCtx) SetMsg(msg string) *SubmitCtx {
	ctx.Msg = msg
	ctx.LastUpdate = time.Now().UnixNano()
	return ctx
}

// Problem 问题定义
type Problem struct {
	Version  int        `yaml:"version"`
	Id       string     `yaml:"id"`
	Text     string     `yaml:"text"`
	Weight   float64    `yaml:"weight"`
	Submits  []Submit   `yaml:"submits"`
	Workflow []Workflow `yaml:"workflow"`

	// RankUpdate 控制排行榜在新提交完成时如何写入 User.BestSubmits / BestScores。
	// 取值（大小写不敏感）：
	//   ""、"best" —— 默认。仅当 newScore > oldBest 时才覆盖（按最高分排名）。
	//   "always"、"latest" —— 强制覆盖：只要新提交比当前记录的 BestSubmitDate 更晚，
	//                          就用它替换最佳成绩（按最新一次成功提交排名）。
	// 任何无法识别的值都退回到 "best" 语义。失败 / 未完成的提交始终不会写入。
	RankUpdate string `yaml:"rankupdate,omitempty"`

	// Package: import-time only metadata, ignored by the runtime evaluator.
	Package *PackageSpec `yaml:"package,omitempty"`
}

// PackageSpec 描述如何把一个 problem 目录部署到 SOJ。
// 由 `soj import` 解析；runtime 不读这一段。
type PackageSpec struct {
	Image    *PackageImage `yaml:"image,omitempty"`
	Scaffold []string      `yaml:"scaffold,omitempty"` // 相对包根的路径；以 / 结尾视为目录递归
}

type PackageImage struct {
	Def string `yaml:"def,omitempty"` // Apptainer .def 文件（相对包根）
	Sif string `yaml:"sif,omitempty"` // 直接给一个已构建好的 .sif（相对包根，与 Def 二选一）
}

// Submit 提交定义
type Submit struct {
	Path  string `yaml:"path"`
	IsDir bool   `yaml:"isdir"`
}

// Workflow 工作流定义
type Workflow struct {
	Image           string   `yaml:"image"`
	Steps           []string `yaml:"steps"`
	Timeout         int      `yaml:"timeout"`
	DisableNetwork  bool     `yaml:"disablenetwork"`
	Show            []int    `yaml:"show"`
	PrivilegedSteps []int    `yaml:"privilegedsteps"`
	NetworkHostMode bool     `yaml:"networkhostmode"`
	Mounts          []Mount  `yaml:"mounts"`

	// Mask 开关：true 时启用路径屏蔽。MaskFiles/MaskDirs 若非空则覆盖 Config 中的默认值。
	Mask      bool     `yaml:"mask"`
	MaskFiles []string `yaml:"maskfiles"`
	MaskDirs  []string `yaml:"maskdirs"`

	// Properties 直通给 systemd-run 的 --property=KEY=VALUE，每条字符串一个。
	// 例：["AllowedCPUs=0-31", "AllowedMemoryNodes=0", "MemoryMax=64G", "CPUQuota=3200%"]
	// 仅 scope 单元支持的 cgroup/超时类属性可用。若为 nil 则回退到 Config.DefaultProperties。
	Properties []string `yaml:"properties"`

	// User 控制容器内非特权步骤运行时的身份。
	// "" 表示 Config.SubmitUid；"root"/"0" 表示容器内 root（也等价于将所有步骤标记为
	// privileged）；其它为数字 uid（gid 与之相同）。PrivilegedSteps 中列出的步骤
	// 始终以容器 root 运行，忽略本字段。
	User string `yaml:"user"`

	// 安全配置（在 apptainer instance start 时一次性生效，所有步骤共享同一信封）。
	// NoPrivs 等价 apptainer --no-privs（丢弃所有 cap + 设置 NoNewPrivs）。
	// AddCaps 在 DropCaps/NoPrivs 之后追加回来；evaluator 在存在非特权步骤时会自动
	// 把 CAP_SETUID/CAP_SETGID 追加进来，否则容器内 setpriv 无法切 uid。
	NoPrivs   bool     `yaml:"noprivs"`
	KeepPrivs bool     `yaml:"keepprivs"`
	DropCaps  []string `yaml:"dropcaps"`
	AddCaps   []string `yaml:"addcaps"`

	// Seccomp 指向宿主机上的 OCI 格式 seccomp profile（JSON）。""时回退到
	// Config.DefaultSeccomp；NoSeccomp=true 时显式禁用默认 profile。
	Seccomp   string `yaml:"seccomp"`
	NoSeccomp bool   `yaml:"noseccomp"`
}

// Mount 挂载定义
type Mount struct {
	Type     string `yaml:"type"`
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"readonly"`
}

// User 用户信息
type User struct {
	ID             string         `gorm:"primaryKey" json:"id"`
	Token          string         `gorm:"uniqueIndex" json:"-"`
	BestScores     JMapStrFloat64 `json:"best_scores"`
	BestSubmits    JMapStrString  `json:"best_submits"`
	BestSubmitDate JMapStrInt64   `json:"best_submit_date"`
	BestTags       JMapStrString  `json:"best_tags"`
	TotalScore     float64        `json:"total_score"`
}

func (u *User) CalculateTotalScore() {
	var total float64
	for _, s := range u.BestScores {
		total += s
	}
	u.TotalScore = total
}

// 辅助函数
func GetTime(t time.Time) aurora.Value {
	return aurora.Gray(15, t.Format("2006-01-02 15:04:05.000"))
}

func ColorizeScore(res JudgeResult) aurora.Value {
	if !res.Success {
		return aurora.Gray(15, res.Score)
	}
	if res.Score >= 95 {
		return aurora.Green(res.Score)
	} else if res.Score >= 60 {
		return aurora.Yellow(res.Score)
	} else {
		return aurora.Red(res.Score)
	}
}

func ColorizeStatus(status string) aurora.Value {
	switch status {
	case "init":
		return aurora.Gray(10, status)
	case "queued":
		return aurora.Cyan(status)
	case "running":
		return aurora.Yellow(status)
	case "prep_dirs":
		return aurora.Yellow(status)
	case "prep_files":
		return aurora.Yellow(status)
	case "run_workflow":
		return aurora.Yellow(status)
	case "collect_result":
		return aurora.Yellow(status)
	case "completed":
		return aurora.Green(status)
	case "failed":
		return aurora.Red(status)
	case "dead":
		return aurora.Gray(15, status)
	default:
		return aurora.Bold(status)
	}
}

// 数据库类型定义
type JMapStrFloat64 map[string]float64
type JMapStrString map[string]string
type JMapStrInt64 map[string]int64
type SubmitsHashes []SubmitHash
type WorkflowResults []WorkflowResult

// 数据库序列化接口实现
func (sh SubmitHash) Value() (driver.Value, error) {
	return json.Marshal(sh)
}

func (sh *SubmitHash) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, sh)
	}
	return json.Unmarshal(b, sh)
}

func (sh SubmitsHashes) Value() (driver.Value, error) {
	return json.Marshal(sh)
}

func (sh *SubmitsHashes) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, sh)
	}
	return json.Unmarshal(b, sh)
}

func (sh WorkflowResult) Value() (driver.Value, error) {
	return json.Marshal(sh)
}

func (sh *WorkflowResult) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, sh)
	}
	return json.Unmarshal(b, sh)
}

func (sh WorkflowResults) Value() (driver.Value, error) {
	return json.Marshal(sh)
}

func (sh *WorkflowResults) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, sh)
	}
	return json.Unmarshal(b, sh)
}

func (sh JudgeResult) Value() (driver.Value, error) {
	return json.Marshal(sh)
}

func (sh *JudgeResult) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, sh)
	}
	return json.Unmarshal(b, sh)
}

func (sh Userface) Value() (driver.Value, error) {
	return sh.Buffer.String(), nil
}

func (sh *Userface) Scan(value interface{}) error {
	b, ok := value.(string)
	if !ok {
		return json.Unmarshal([]byte(b), sh)
	}
	sh.Buffer = bytes.NewBufferString(b)
	return nil
}

func (u JMapStrFloat64) Value() (driver.Value, error) {
	return json.Marshal(u)
}

func (u *JMapStrFloat64) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, u)
	}
	return json.Unmarshal(b, u)
}

func (u JMapStrString) Value() (driver.Value, error) {
	return json.Marshal(u)
}

func (u *JMapStrString) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, u)
	}
	return json.Unmarshal(b, u)
}

func (u JMapStrInt64) Value() (driver.Value, error) {
	return json.Marshal(u)
}

func (u *JMapStrInt64) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return json.Unmarshal(b, u)
	}
	return json.Unmarshal(b, u)
}
