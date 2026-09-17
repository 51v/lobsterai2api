// Package upstream 封装对 LobsterAI 上游的全部 HTTP 调用。
package upstream

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"lobsterai2api/internal/auth"
)

const (
	defaultClientVersion = "0.1.0"
	clientUA             = "LobsterAI/0.1.0"
	updateAPIURL         = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"
)

var (
	clientVersionMu     sync.Mutex
	clientVersionCached string
	clientVersionAt     time.Time
)

// ServerBase returns the upstream API base URL from LB2A_UPSTREAM_BASE env.
// No hardcoded domain — users must set this in their config or environment.
func ServerBase() string {
	if v := os.Getenv("LB2A_UPSTREAM_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return ""
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。
type Client struct {
	HTTP     *http.Client
	LastBody []byte // 最近一次非 2xx 响应体，供调用方 Classify
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP: &http.Client{Timeout: 180 * time.Second, Transport: tr},
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.LastBody = raw
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// chatHeaders 设置 chat completions 请求头。
func chatHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-LobsterAI-Client-Capabilities", "kimi-k3-agentic-v1")
	req.Header.Set("X-LobsterAI-Client-Version", resolveClientVersion())
}

// authHeaders 设置 auth 请求头（exchange/refresh 不需要 Bearer token）。
func authHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
func (c *Client) RefreshToken(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := ServerBase() + "/api/auth/refresh"
	body := a.KeyfromBody()
	body["refreshToken"] = a.RefreshToken
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	authHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	} else if exp := jwtExpiry(tok.AccessToken); exp > 0 {
		a.ExpiresAt = exp
	}
	return nil
}

// jwtExpiry 解码 JWT payload 的 exp（Unix 秒）；失败返回 0。
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp
}

// prepareChatBody 预处理请求体：force stream=true（上游只支持流式，实测 stream:false 返回 500），
// 标准化 tool_choice。
func prepareChatBody(rawBody []byte) []byte {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody // 解析失败原样发送
	}
	// force stream for upstream SSE compat
	body["stream"] = true
	// normalize tool_choice
	if tc, ok := body["tool_choice"]; ok {
		switch v := tc.(type) {
		case string:
			if v == "" || v == "none" {
				delete(body, "tool_choice")
			}
		case map[string]any:
			// object form, keep as-is
		case nil:
			delete(body, "tool_choice")
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return rawBody
	}
	return out
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、status 为上游状态码、err 为 nil（body 在 c.LastBody，
// 调用方用 Classify(status, body) 判定）；只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, err error) {
	url := ServerBase() + "/api/proxy/v1/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepareChatBody(body)))
	if err != nil {
		return nil, 0, err
	}
	chatHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		c.LastBody = raw
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, nil
	}
	return resp.Body, resp.StatusCode, nil
}

// FetchModels 调上游动态模型接口。
// GET {server}/api/models/available，Bearer accessToken。
// 返回模型 ID 列表；失败返回错误（调用方回退静态表）。
func (c *Client) FetchModels(a *auth.Auth) ([]string, error) {
	url := ServerBase() + "/api/models/available"
	body := a.KeyfromBody()
	// build query string from keyfrom
	parts := make([]string, 0)
	for k, v := range body {
		parts = append(parts, fmt.Sprintf("%s=%s", k, fmt.Sprintf("%v", v)))
	}
	if len(parts) > 0 {
		url += "?" + strings.Join(parts, "&")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data []struct {
			ModelID   string `json:"modelId"`
			ModelName string `json:"modelName"`
			Provider  string `json:"provider"`
			ApiFormat string `json:"apiFormat"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	ids := make([]string, 0, len(env.Data))
	for _, m := range env.Data {
		if m.ModelID != "" {
			ids = append(ids, m.ModelID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return ids, nil
}

// QuotaUsage 查询账号当前积分。
// GET {server}/api/user/profile-summary 的 totalCreditsRemaining（含 free + campaign 活动积分）。
// 注意: /api/user/quota 只显示 freeCreditsTotal=300, 不含 5000 活动积分。
func (c *Client) QuotaUsage(a *auth.Auth) (remain int64, total int64, err error) {
	url := ServerBase() + "/api/user/profile-summary"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	var ps struct {
		TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
	}
	if err := json.Unmarshal(data, &ps); err != nil {
		return 0, 0, fmt.Errorf("profile-summary parse: %w", err)
	}
	clamp := func(v float64) int64 {
		if v < 0 {
			return 0
		}
		return int64(v)
	}
	if ps.TotalCreditsRemaining > 0 {
		return clamp(ps.TotalCreditsRemaining), 0, nil
	}
	return 0, 0, fmt.Errorf("profile-summary: no credits")
}

// resolveClientVersion 拿官方最新客户端版本号（缓存 23h，失败回退默认值）。
func resolveClientVersion() string {
	clientVersionMu.Lock()
	defer clientVersionMu.Unlock()
	if clientVersionCached != "" && time.Since(clientVersionAt) < 23*time.Hour {
		return clientVersionCached
	}
	v, err := fetchClientVersion()
	if err != nil {
		log.Printf("resolveClientVersion failed: %v, using default", err)
		v = defaultClientVersion
	}
	clientVersionCached = v
	clientVersionAt = time.Now()
	return v
}

func fetchClientVersion() (string, error) {
	req, err := http.NewRequest(http.MethodGet, updateAPIURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept", "application/json")
	cli := &http.Client{Timeout: 15 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update api status %d", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Value struct {
				Version string `json:"version"`
			} `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("parse version: %w", err)
	}
	if env.Data.Value.Version == "" {
		return "", fmt.Errorf("empty version")
	}
	return env.Data.Value.Version, nil
}

// DailyCheckin 对所有注册账号执行每日签到（每天每个号 +100 积分）。
// 流程：slot → context（判 claimedToday + check_in action）→ POST actions/check_in。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("no accessToken")
	}

	ver := resolveClientVersion()
	ua := "LobsterAI/" + ver
	base := ServerBase()

	// 1) 轮询签到 slot
	q := "placement=desktop_sidebar&clientVersion=" + ver + "&containerApiVersion=2&platform=win32"
	slotURL := base + "/api/client-activities/slot?" + q
	req, _ := http.NewRequest(http.MethodGet, slotURL, nil)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	data, err := c.doJSON(req)
	if err != nil {
		return fmt.Errorf("slot: %w", err)
	}
	var slot struct {
		SlotState string `json:"slotState"`
		Activity  struct {
			ActivityCode   string `json:"activityCode"`
			ConfigRevision int    `json:"configRevision"`
		} `json:"activity"`
	}
	if err := json.Unmarshal(data, &slot); err != nil {
		return fmt.Errorf("slot parse: %w", err)
	}
	if slot.SlotState != "available" || slot.Activity.ActivityCode == "" {
		return fmt.Errorf("no check-in slot (state=%s)", slot.SlotState)
	}
	code := slot.Activity.ActivityCode
	rev := slot.Activity.ConfigRevision

	// 2) 查上下文（判断是否今天已签，有哪些 action 可用）
	ctxURL := fmt.Sprintf("%s/api/client-activities/%s/context?configRevision=%d", base, code, rev)
	req, _ = http.NewRequest(http.MethodGet, ctxURL, nil)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	raw, err := c.doJSON(req)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	var ctx struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
		} `json:"state"`
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(raw, &ctx); err != nil {
		return fmt.Errorf("context parse: %w", err)
	}
	if ctx.State.ClaimedToday {
		return nil // 今天已签过，不算错
	}
	hasAction := false
	for _, act := range ctx.Actions {
		if act == "check_in" {
			hasAction = true
			break
		}
	}
	if !hasAction {
		return fmt.Errorf("check_in action not available")
	}

	// 3) 执行签到
	idempotencyKey := uuid4()
	checkinBody := map[string]any{
		"configRevision": rev,
		"idempotencyKey": idempotencyKey,
		"payload":        map[string]any{},
	}
	checkinBytes, _ := json.Marshal(checkinBody)
	checkinURL := fmt.Sprintf("%s/api/client-activities/%s/actions/check_in", base, code)
	req, _ = http.NewRequest(http.MethodPost, checkinURL, bytes.NewReader(checkinBytes))
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	data, err = c.doJSON(req)
	if err != nil {
		return fmt.Errorf("check_in: %w", err)
	}
	var result struct {
		Result struct {
			CreditsGranted float64 `json:"creditsGranted"`
			RewardCredits  float64 `json:"rewardCredits"`
			Credits        float64 `json:"credits"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("check_in parse: %w", err)
	}
	gained := result.Result.CreditsGranted
	if gained == 0 {
		gained = result.Result.RewardCredits
	}
	if gained == 0 {
		gained = result.Result.Credits
	}
	if gained > 0 {
		log.Printf("checkin %s: ✅ +%.0f 积分", a.UID, gained)
	}
	return nil
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
