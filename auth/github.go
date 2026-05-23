package auth

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/schollz/progressbar/v3"
	gossh "golang.org/x/crypto/ssh"
)

const (
	defaultEndpoint = "https://github.com"
	httpTimeout     = 10 * time.Second
	maxConcurrent   = 10
)

// FetchResult 批量拉取结果
type FetchResult struct {
	Keys        map[string][]gossh.PublicKey // username → 公钥列表
	TotalUsers  int                          // 请求的用户总数
	LoadedUsers int                          // 成功加载的用户数
	FailedUsers []string                     // 拉取失败的用户名
	TotalKeys   int                          // 加载的公钥总数
}

// keysURL 根据 endpoint 构造 .keys URL
func keysURL(endpoint, username string) string {
	return fmt.Sprintf("%s/%s.keys", endpoint, username)
}

// FetchGitHubKeys 从 GitHub endpoint 拉取指定用户的 SSH 公钥
func FetchGitHubKeys(ctx context.Context, endpoint, username, token string) ([]gossh.PublicKey, error) {
	url := keysURL(endpoint, username)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var keys []gossh.PublicKey
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		pk, _, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			log.Debug().Str("user", username).Err(err).Msg("skipping unparseable key")
			continue
		}
		keys = append(keys, pk)
	}
	return keys, scanner.Err()
}

// FetchAllKeysForUsers 并发拉取多个用户的 GitHub 公钥，带进度条
func FetchAllKeysForUsers(ctx context.Context, endpoint, token string, usernames []string) *FetchResult {
	result := &FetchResult{
		Keys:       make(map[string][]gossh.PublicKey),
		TotalUsers: len(usernames),
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)

	var done atomic.Int32

	bar := progressbar.NewOptions(len(usernames),
		progressbar.OptionSetDescription("  Fetching SSH keys"),
		progressbar.OptionShowCount(),
		progressbar.OptionSetWidth(40),
		progressbar.OptionClearOnFinish(),
	)

	for _, u := range usernames {
		wg.Add(1)
		sem <- struct{}{}
		go func(username string) {
			defer wg.Done()
			defer func() { <-sem }()

			keys, err := FetchGitHubKeys(ctx, endpoint, username, token)

			mu.Lock()
			defer mu.Unlock()

			done.Add(1)
			bar.Add(1)

			if err != nil {
				log.Error().Str("user", username).Err(err).Msg("fetch failed")
				result.FailedUsers = append(result.FailedUsers, username)
				return
			}
			if len(keys) == 0 {
				log.Warn().Str("user", username).Msg("no SSH keys found")
				result.FailedUsers = append(result.FailedUsers, username)
				return
			}

			result.Keys[username] = keys
			result.LoadedUsers++
			result.TotalKeys += len(keys)
		}(u)
	}

	wg.Wait()
	bar.Finish()

	return result
}
