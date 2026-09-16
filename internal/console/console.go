// Package console 内置 Web 管理台：添加账号（OAuth 回填）+ 账号数据展示。
// 挂载在主服务的 /console 路径下，复用同一 api_key 鉴权。
package console

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/reqlog"
	"lobsterai2api/internal/upstream"
)

const clientUA = "LobsterAI/0.1.0"

// Config 控制台依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 与主 API 相同的鉴权 key
	AuthDir   string // auths 目录
	PortalURL string // 登录门户，如 https://lobsterai.youdao.com
	ReqLog    *reqlog.Logger // 请求日志（可空）
}

// pendingLogin 一次进行中的授权。
type pendingLogin struct {
	UUID         string
	FirstKeyfrom string
	CreatedAt    time.Time
}

// Console 管理台 HTTP handler。
type Console struct {
	cfg     Config
	mu      sync.Mutex
	pending map[string]*pendingLogin // state → login
	index   []byte
}

// New 构建控制台。
func New(cfg Config) (*Console, error) {
	c := &Console{cfg: cfg, pending: map[string]*pendingLogin{}}
	raw, err := indexHTML()
	if err != nil {
		return nil, err
	}
	c.index = raw
	return c, nil
}

// ServeHTTP 路由：/console 及其子路径。
func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/console")
	p = "/" + strings.TrimPrefix(p, "/")

	switch {
	case p == "/" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(c.index)

	case p == "/api/login/start" && r.Method == http.MethodPost:
		c.auth(c.loginStart)(w, r)

	case p == "/api/login/callback" && r.Method == http.MethodPost:
		c.auth(c.loginCallback)(w, r)

	case p == "/api/accounts" && r.Method == http.MethodGet:
		c.auth(c.accounts)(w, r)

	case p == "/api/refresh-credits" && r.Method == http.MethodPost:
		c.auth(c.refreshCredits)(w, r)

	case p == "/api/models" && r.Method == http.MethodGet:
		c.auth(c.models)(w, r)

	case p == "/api/logs" && r.Method == http.MethodGet:
		c.auth(c.requestLogs)(w, r)

	case p == "/api/account/delete" && r.Method == http.MethodPost:
		c.auth(c.accountDelete)(w, r)

	case p == "/api/account/refresh-token" && r.Method == http.MethodPost:
		c.auth(c.accountRefreshToken)(w, r)

	case p == "/api/account/enable" && r.Method == http.MethodPost:
		c.auth(c.accountEnable)(w, r)

	case p == "/api/account/disable" && r.Method == http.MethodPost:
		c.auth(c.accountDisable)(w, r)

	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// auth 鉴权：Bearer api_key（与主 API 一致），或 ?key= 查询参数（便于浏览器直接打开）。
func (c *Console) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.cfg.APIKey != "" {
			token := ""
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				token = strings.TrimPrefix(h, "Bearer ")
			}
			if token == "" {
				token = r.URL.Query().Get("key")
			}
			if token != c.cfg.APIKey {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid key"})
				return
			}
		}
		next(w, r)
	}
}

// loginStart 生成一次授权：返回登录链接（浏览器打开登录，回调地址打不开是正常的）。
func (c *Console) loginStart(w http.ResponseWriter, r *http.Request) {
	state := randomHex(16)
	uuid := newUuid()
	first := fmt.Sprintf("%d", time.Now().UnixMilli())

	c.mu.Lock()
	// 清理超过 15 分钟的 pending
	for k, v := range c.pending {
		if time.Since(v.CreatedAt) > 15*time.Minute {
			delete(c.pending, k)
		}
	}
	c.pending[state] = &pendingLogin{UUID: uuid, FirstKeyfrom: first, CreatedAt: time.Now()}
	c.mu.Unlock()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/auth/callback", 30000+int(time.Now().UnixMilli()%20000))
	loginURL := fmt.Sprintf("%s/portal#/login?source=electron&redirect_uri=%s&state=%s",
		strings.TrimRight(c.cfg.PortalURL, "/"), url.QueryEscape(redirectURI), state)

	writeJSON(w, http.StatusOK, map[string]any{
		"login_url":     loginURL,
		"state":         state,
		"expires_in_s":  900,
		"hint":          "浏览器打开链接完成登录后，把跳转到的 127.0.0.1 回调完整 URL 粘贴回来",
	})
}

// loginCallback 接收回调 URL，完成 exchange 并入池。
func (c *Console) loginCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing url"})
		return
	}
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad url"})
		return
	}
	code := u.Query().Get("code")
	state := u.Query().Get("state")
	if code == "" || state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "url 里缺 code/state（要整条回调 URL）"})
		return
	}

	c.mu.Lock()
	pl := c.pending[state]
	if pl != nil {
		delete(c.pending, state)
	}
	c.mu.Unlock()
	if pl == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state 无效或已过期，请重新开始授权"})
		return
	}

	a, err := c.exchange(code, pl)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("exchange 失败: %v", err)})
		return
	}

	// 落盘
	if err := os.MkdirAll(c.cfg.AuthDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	fp := filepath.Join(c.cfg.AuthDir, fmt.Sprintf("lobsterai-%s.json", a.UID))
	a.FilePath = fp // SaveAtomic 依赖 FilePath，必须先赋值
	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 入池 + 立即查一次积分
	c.cfg.Pool.Add(a)
	credits := int64(0)
	if remain, _, err := c.cfg.Upstream.QuotaUsage(a); err == nil {
		credits = remain
		c.cfg.Pool.SetCredits(a.UID, remain)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"uid":      a.UID,
		"nickname": a.Nickname,
		"credits":  credits,
		"file":     filepath.Base(fp),
	})
}

// exchange 用 authCode 换 token（与 cmd/login 相同协议）。
func (c *Console) exchange(code string, pl *pendingLogin) (*auth.Auth, error) {
	body := map[string]any{
		"authCode":      code,
		"firstKeyfrom":  pl.FirstKeyfrom,
		"latestKeyfrom": fmt.Sprintf("%d", time.Now().UnixMilli()),
		"uuid":          pl.UUID,
		"version":       "0.1.0",
	}
	raw, _ := json.Marshal(body)
	url := strings.TrimRight(upstream.ServerBase(), "/") + "/api/auth/exchange"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.cfg.Upstream.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(data), 160))
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	var ex struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		User         struct {
			ID       string `json:"id"`
			Yid      string `json:"yid"`
			UserId   string `json:"userId"`
			Nickname string `json:"nickname"`
		} `json:"user"`
	}
	if err := json.Unmarshal(env.Data, &ex); err != nil {
		return nil, err
	}
	if ex.AccessToken == "" {
		return nil, fmt.Errorf("响应里没有 accessToken")
	}
	uid := ex.User.ID
	if uid == "" {
		uid = ex.User.UserId
	}
	if uid == "" {
		uid = ex.User.Yid
	}
	if uid == "" {
		uid = fmt.Sprintf("%x", sha256.Sum256([]byte(ex.AccessToken)))[:16]
	}
	expiresAt := int64(0)
	if ex.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(ex.ExpiresIn) * time.Second).Unix()
	} else if exp := jwtExpiry(ex.AccessToken); exp > 0 {
		expiresAt = exp
	}
	return &auth.Auth{
		AccessToken:   ex.AccessToken,
		RefreshToken:  ex.RefreshToken,
		ExpiresAt:     expiresAt,
		UID:           uid,
		UserId:        ex.User.UserId,
		Nickname:      ex.User.Nickname,
		Uuid:          pl.UUID,
		FirstKeyfrom:  pl.FirstKeyfrom,
		LatestKeyfrom: fmt.Sprintf("%d", time.Now().UnixMilli()),
	}, nil
}

// accountView 展示用账号视图。
type accountView struct {
	pool.Status
	TokenExpiresAt int64  `json:"token_expires_at"`
	ExpiresHuman    string `json:"expires_human"`
}

// accounts 账号列表。
func (c *Console) accounts(w http.ResponseWriter, r *http.Request) {
	list := c.cfg.Pool.List()
	out := make([]accountView, 0, len(list))
	for _, s := range list {
		v := accountView{Status: s}
		if a := c.cfg.Pool.AuthByUID(s.UID); a != nil {
			v.TokenExpiresAt = a.ExpiresAt
			if a.ExpiresAt > 0 {
				v.ExpiresHuman = time.Unix(a.ExpiresAt, 0).Format("2006-01-02 15:04")
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// refreshCredits 对所有账号立刻刷一次余额。
func (c *Console) refreshCredits(w http.ResponseWriter, r *http.Request) {
	list := c.cfg.Pool.List()
	type result struct {
		UID   string `json:"uid"`
		Credits *int64 `json:"credits"`
		Error string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(list))
	for _, s := range list {
		a := c.cfg.Pool.AuthByUID(s.UID)
		if a == nil {
			results = append(results, result{UID: s.UID, Error: "no auth"})
			continue
		}
		remain, _, err := c.cfg.Upstream.QuotaUsage(a)
		if err != nil {
			results = append(results, result{UID: s.UID, Error: truncate(err.Error(), 120)})
			continue
		}
		c.cfg.Pool.SetCredits(s.UID, remain)
		results = append(results, result{UID: s.UID, Credits: &remain})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// models 模型列表（转发主 handler 同款逻辑，直接用 upstream）。
func (c *Console) models(w http.ResponseWriter, r *http.Request) {
	acct := c.cfg.Pool.Pick()
	if acct == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": []string{}})
		return
	}
	ids, err := c.cfg.Upstream.FetchModels(acct)
	if err != nil {
		ids = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}

// requestLogs 最近请求日志。
func (c *Console) requestLogs(w http.ResponseWriter, r *http.Request) {
	if c.cfg.ReqLog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"logs": []*reqlog.Entry{}, "enabled": false})
		return
	}
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 500 {
			n = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": c.cfg.ReqLog.Recent(n), "enabled": true})
}

// accountDelete 删除账号：内存池 + 状态 + 凭据文件。
func (c *Console) accountDelete(w http.ResponseWriter, r *http.Request) {
	uid, ok := c.readUID(w, r)
	if !ok {
		return
	}
	if c.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "账号不存在"})
		return
	}
	a := c.cfg.Pool.AuthByUID(uid)
	c.cfg.Pool.Remove(uid)
	deleted := false
	if a != nil && a.FilePath != "" {
		if err := os.Remove(a.FilePath); err == nil {
			deleted = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "file_deleted": deleted})
}

// accountRefreshToken 手动刷新某账号 token（继承 upstream.RefreshToken + 落盘）。
func (c *Console) accountRefreshToken(w http.ResponseWriter, r *http.Request) {
	uid, ok := c.readUID(w, r)
	if !ok {
		return
	}
	a := c.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "账号不存在"})
		return
	}
	if strings.TrimSpace(a.RefreshToken) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无 refreshToken，需重新授权"})
		return
	}
	if err := c.cfg.Upstream.RefreshToken(a); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": truncate(err.Error(), 160)})
		return
	}
	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "刷新成功但落盘失败: " + err.Error()})
		return
	}
	expires := ""
	if a.ExpiresAt > 0 {
		expires = time.Unix(a.ExpiresAt, 0).Format("2006-01-02 15:04:05")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "expires": expires})
}

// accountEnable 启用/解冻账号。
func (c *Console) accountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := c.readUID(w, r)
	if !ok {
		return
	}
	if c.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "账号不存在"})
		return
	}
	c.cfg.Pool.Reenable(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

// accountDisable 禁用账号（暂停使用，不删数据）。
func (c *Console) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := c.readUID(w, r)
	if !ok {
		return
	}
	if c.cfg.Pool.AuthByUID(uid) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "账号不存在"})
		return
	}
	c.cfg.Pool.DisableUID(uid, "手动禁用")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

// readUID 从 JSON body 读 uid。
func (c *Console) readUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		UID string `json:"uid"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.UID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return "", false
	}
	return strings.TrimSpace(req.UID), true
}

// ---------------------------------------------------------------------------

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newUuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

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

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
