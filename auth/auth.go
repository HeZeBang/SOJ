package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	ssh "github.com/gliderlabs/ssh"
	"github.com/mrhaoxx/SOJ/types"
	"github.com/rs/zerolog/log"
	gossh "golang.org/x/crypto/ssh"
)

// fingerprint 计算公钥的 SHA256 指纹
func fingerprint(key gossh.PublicKey) string {
	h := sha256.Sum256(key.Marshal())
	return fmt.Sprintf("SHA256:%x", h[:])
}

// AuthManager 管理 SSH 公钥认证
type AuthManager struct {
	cfg      types.AuthConfig
	endpoint string // GitHub endpoint，来自 cfg.GitHubEndpoint 或默认值

	mu        sync.RWMutex
	keyToUser map[string]string // fingerprint → username
}

// NewAuthManager 创建 AuthManager，根据配置模式加载公钥
func NewAuthManager(cfg types.AuthConfig, legacyPubkey string) (*AuthManager, error) {
	// 向后兼容：如果 Auth.Mode 为空，从旧字段推断
	if cfg.Mode == "" {
		if legacyPubkey != "" {
			cfg.Mode = "single"
			cfg.AllowedSSHPubkey = legacyPubkey
		} else if len(cfg.GitHubUsers) > 0 {
			cfg.Mode = "github-list"
		} else {
			cfg.Mode = "open"
		}
	}

	// 向后兼容：single 模式下合并旧字段
	if cfg.Mode == "single" && cfg.AllowedSSHPubkey == "" && legacyPubkey != "" {
		cfg.AllowedSSHPubkey = legacyPubkey
	}

	// GitHub endpoint 默认值
	endpoint := cfg.GitHubEndpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	// 去掉末尾斜杠
	endpoint = strings.TrimRight(endpoint, "/")

	m := &AuthManager{
		cfg:       cfg,
		endpoint:  endpoint,
		keyToUser: make(map[string]string),
	}

	if err := m.loadKeys(); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *AuthManager) loadKeys() error {
	switch m.cfg.Mode {
	case "single":
		return m.loadSingle()
	case "github-list":
		return m.loadGitHubList()
	case "open":
		log.Warn().Msg("auth: open mode, accepting any public key")
		return nil
	default:
		return fmt.Errorf("unknown auth mode: %q (valid: single, github-list)", m.cfg.Mode)
	}
}

func (m *AuthManager) loadSingle() error {
	if m.cfg.AllowedSSHPubkey == "" {
		log.Warn().Msg("auth: single mode with no key configured, accepting any")
		return nil
	}

	pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(m.cfg.AllowedSSHPubkey))
	if err != nil {
		return fmt.Errorf("parse AllowedSSHPubkey: %w", err)
	}

	fp := fingerprint(pk)
	m.mu.Lock()
	m.keyToUser[fp] = "*" // 通配：single 模式下 key 匹配即放行，不限制用户名
	m.mu.Unlock()

	log.Info().Str("mode", "single").Str("fingerprint", fp).Msg("auth: single key loaded")
	return nil
}

func (m *AuthManager) loadGitHubList() error {
	if len(m.cfg.GitHubUsers) == 0 {
		return fmt.Errorf("github-list mode requires at least one GitHubUsers entry")
	}

	log.Info().
		Str("endpoint", m.endpoint).
		Int("users", len(m.cfg.GitHubUsers)).
		Msg("auth: fetching SSH keys from GitHub...")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(m.cfg.GitHubUsers))*10*time.Second+30*time.Second)
	defer cancel()

	result := FetchAllKeysForUsers(ctx, m.endpoint, m.cfg.GitHubToken, m.cfg.GitHubUsers)

	// 写入密钥映射
	m.mu.Lock()
	totalKeys := 0
	for username, userKeys := range result.Keys {
		for _, pk := range userKeys {
			fp := fingerprint(pk)
			m.keyToUser[fp] = username
			totalKeys++
		}
	}
	m.mu.Unlock()

	// 打印汇总
	log.Info().
		Str("mode", "github-list").
		Int("total_users", result.TotalUsers).
		Int("loaded", result.LoadedUsers).
		Int("keys", totalKeys).
		Msg("auth: key loading complete")

	if len(result.FailedUsers) > 0 {
		log.Warn().
			Strs("users", result.FailedUsers).
			Int("count", len(result.FailedUsers)).
			Msg("auth: some users failed to load (they will not be able to login)")
	}

	// 非致命：即使全部失败也继续运行，仅警告
	if totalKeys == 0 {
		log.Error().Msg("auth: WARNING - no SSH keys loaded, nobody will be able to login until keys are refreshed")
	}

	return nil
}

// PublicKeyHandler 返回用于 SSH 服务器的公钥认证处理函数
func (m *AuthManager) PublicKeyHandler() ssh.PublicKeyHandler {
	return func(ctx ssh.Context, key ssh.PublicKey) bool {
		if m.cfg.Mode == "open" {
			return true
		}

		fp := fingerprint(key)

		m.mu.RLock()
		mapped, ok := m.keyToUser[fp]
		m.mu.RUnlock()

		if !ok {
			log.Debug().
				Str("user", ctx.User()).
				Str("fingerprint", fp).
				Msg("auth: unknown key rejected")
			return false
		}

		// 通配模式（single）：key 匹配即放行
		if mapped == "*" {
			return true
		}

		// github-list 模式：key 必须属于声称的用户名
		if mapped != ctx.User() {
			log.Warn().
				Str("claimed_user", ctx.User()).
				Str("actual_user", mapped).
				Str("fingerprint", fp).
				Msg("auth: key belongs to different user, rejected")
			return false
		}

		return true
	}
}

// Refresh 重新从 GitHub 拉取所有密钥（仅 github-list 模式有效）
func (m *AuthManager) Refresh() error {
	if m.cfg.Mode != "github-list" {
		return fmt.Errorf("refresh only available in github-list mode (current: %s)", m.cfg.Mode)
	}

	log.Info().
		Str("endpoint", m.endpoint).
		Int("users", len(m.cfg.GitHubUsers)).
		Msg("auth: refreshing SSH keys from GitHub...")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(m.cfg.GitHubUsers))*10*time.Second+30*time.Second)
	defer cancel()

	result := FetchAllKeysForUsers(ctx, m.endpoint, m.cfg.GitHubToken, m.cfg.GitHubUsers)

	newMap := make(map[string]string)
	totalKeys := 0
	for username, userKeys := range result.Keys {
		for _, pk := range userKeys {
			fp := fingerprint(pk)
			newMap[fp] = username
			totalKeys++
		}
	}

	m.mu.Lock()
	m.keyToUser = newMap
	m.mu.Unlock()

	log.Info().
		Int("loaded", result.LoadedUsers).
		Int("failed", len(result.FailedUsers)).
		Int("keys", totalKeys).
		Msg("auth: keys refreshed")

	if len(result.FailedUsers) > 0 {
		log.Warn().Strs("users", result.FailedUsers).Msg("auth: refresh - users that failed")
	}

	if totalKeys == 0 {
		return fmt.Errorf("refresh resulted in zero keys loaded")
	}
	return nil
}

// GetMode 返回当前认证模式
func (m *AuthManager) GetMode() string {
	return m.cfg.Mode
}
