package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
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

	cacheMu sync.Mutex // 保护 saveCache 文件写入
}

type keyCache struct {
	UpdatedAt time.Time         `json:"updated_at"`
	Keys      map[string]string `json:"keys"` // fingerprint → username
}

const cacheMaxAge = 24 * time.Hour

func hasGitHubConfig(cfg types.AuthConfig) bool {
	return len(cfg.GitHubUsers) > 0 ||
		cfg.GitHubToken != "" ||
		cfg.GitHubEndpoint != "" ||
		cfg.KeyCachePath != ""
}

func isValidGitHubUsername(username string) bool {
	if len(username) == 0 || len(username) > 39 {
		return false
	}
	if username[0] == '-' || username[len(username)-1] == '-' {
		return false
	}
	for i := 0; i < len(username); i++ {
		c := username[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

func (m *AuthManager) cachePath() string {
	if m.cfg.KeyCachePath != "" {
		return m.cfg.KeyCachePath
	}
	return "keys_cache.json"
}

func (m *AuthManager) loadCache() (map[string]string, bool) {
	path := m.cachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cache keyCache
	if err := json.Unmarshal(data, &cache); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("auth: corrupt key cache, ignoring")
		return nil, false
	}
	if time.Since(cache.UpdatedAt) > cacheMaxAge {
		log.Info().Time("updated_at", cache.UpdatedAt).Msg("auth: key cache expired, will refetch")
		return nil, false
	}
	if len(cache.Keys) == 0 {
		return nil, false
	}
	log.Info().
		Str("path", path).
		Time("updated_at", cache.UpdatedAt).
		Int("keys", len(cache.Keys)).
		Msg("auth: loaded keys from cache")
	return cache.Keys, true
}

func (m *AuthManager) saveCache(keys map[string]string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	cache := keyCache{
		UpdatedAt: time.Now(),
		Keys:      keys,
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		log.Error().Err(err).Msg("auth: failed to marshal key cache")
		return
	}
	path := m.cachePath()
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Error().Err(err).Str("path", path).Msg("auth: failed to write key cache")
		return
	}
	log.Info().Str("path", path).Int("keys", len(keys)).Msg("auth: key cache saved")
}

// NewAuthManager 创建 AuthManager，根据配置模式加载公钥
func NewAuthManager(cfg types.AuthConfig, legacyPubkey string) (*AuthManager, error) {
	// 向后兼容：如果 Auth.Mode 为空，从旧字段推断
	if cfg.Mode == "" {
		if legacyPubkey != "" {
			cfg.Mode = "single"
			cfg.AllowedSSHPubkey = legacyPubkey
		} else if hasGitHubConfig(cfg) {
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
		return fmt.Errorf("unknown auth mode: %q (valid: single, github-list, open)", m.cfg.Mode)
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
	// 懒加载模式：仅从缓存加载，不触发网络请求
	if cached, ok := m.loadCache(); ok {
		m.mu.Lock()
		m.keyToUser = cached
		m.mu.Unlock()
		return nil
	}

	log.Info().Msg("auth: no key cache found, keys will be fetched on first login")
	return nil
}

// fetchUserKeys 懒加载：从 GitHub 拉取单个用户的公钥并写入 keyToUser
func (m *AuthManager) fetchUserKeys(username string) {
	username = strings.TrimSpace(username)
	if !isValidGitHubUsername(username) {
		log.Warn().Str("user", username).Msg("auth: invalid GitHub username")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	keys, err := FetchGitHubKeys(ctx, m.endpoint, username, m.cfg.GitHubToken)
	if err != nil {
		log.Warn().Str("user", username).Err(err).Msg("auth: lazy fetch failed")
		return
	}
	if len(keys) == 0 {
		log.Warn().Str("user", username).Msg("auth: lazy fetch returned no keys")
		return
	}

	m.mu.Lock()
	for _, pk := range keys {
		fp := fingerprint(pk)
		m.keyToUser[fp] = username
	}
	snapshot := make(map[string]string, len(m.keyToUser))
	for k, v := range m.keyToUser {
		snapshot[k] = v
	}
	m.mu.Unlock()

	m.saveCache(snapshot)
	log.Info().Str("user", username).Int("keys", len(keys)).Msg("auth: lazy fetched keys")
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

		if !ok && m.cfg.Mode == "github-list" {
			m.fetchUserKeys(ctx.User())
			m.mu.RLock()
			mapped, ok = m.keyToUser[fp]
			m.mu.RUnlock()
		}

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

// Refresh 重新从 GitHub 拉取已知用户的密钥（仅 github-list 模式有效）
func (m *AuthManager) Refresh() error {
	if m.cfg.Mode != "github-list" {
		return fmt.Errorf("refresh only available in github-list mode (current: %s)", m.cfg.Mode)
	}

	userSet := make(map[string]struct{})
	for _, username := range m.cfg.GitHubUsers {
		username = strings.TrimSpace(username)
		if username != "" {
			userSet[username] = struct{}{}
		}
	}

	m.mu.RLock()
	for _, username := range m.keyToUser {
		if username != "*" && username != "" {
			userSet[username] = struct{}{}
		}
	}
	m.mu.RUnlock()

	usernames := make([]string, 0, len(userSet))
	for username := range userSet {
		usernames = append(usernames, username)
	}
	if len(usernames) == 0 {
		return fmt.Errorf("no known GitHub users to refresh; users are fetched on first login")
	}

	log.Info().
		Str("endpoint", m.endpoint).
		Int("users", len(usernames)).
		Msg("auth: refreshing SSH keys from GitHub...")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(usernames))*10*time.Second+30*time.Second)
	defer cancel()

	result := FetchAllKeysForUsers(ctx, m.endpoint, m.cfg.GitHubToken, usernames)

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

	// Save to cache
	if totalKeys > 0 {
		m.saveCache(newMap)
	}

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
