// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/reqlog"
	"lobsterai2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string          // 空 = 不鉴权
	MaxRotate    int             // 单请求最多换号次数，默认 3
	HardCooldown time.Duration   // 余额不足冷却，默认 12h
	SoftCooldown time.Duration   // 429 冷却，默认 60s
	ErrThreshold int             // 连续其他错误冷却阈值，默认 3
	ErrCooldown  time.Duration   // 错误冷却时长，默认 10m
	RefreshSkew  time.Duration   // token 提前刷新窗口，默认 10m
	ReqLog       *reqlog.Logger  // 请求日志（可空 = 不记录）
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.status)
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// 静态模型表（动态接口失败时的回退）。
// 2026-08-06 从 GET /api/models/available 实测拉取，共 19 个。
var staticModels = []map[string]any{
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "MiniMax-M3", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "MiniMax-M2.7", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "qwen3.7-max", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "qwen3.7-plus", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "qwen3.6-plus", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "qwen3.5-plus-2026-04-20", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "kimi-k2.7-code-highspeed", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "kimi-k2.5", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "doubao-seed-2-1-pro-260628", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "doubao-seed-2-1-turbo-260628", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "doubao-seed-2-0-code-preview-260215", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
	{"id": "glm-5", "object": "model", "created": 1753600000, "owned_by": "lobsterai", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids     []string
	fetched time.Time
}

const dynamicModelsTTL = time.Hour

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型 ID 列表并包装成 OpenAI 格式。
func (h *Handler) modelList() []map[string]any {
	if ids := h.fetchDynamicModels(); len(ids) > 0 {
		out := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			out = append(out, map[string]any{
				"id":             id,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "lobsterai",
				"context_length": 131072,
			})
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 从池中任一健康账号拉模型列表，缓存 1h。
func (h *Handler) fetchDynamicModels() []string {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		ids := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return ids
	}
	dynamicModelsCache.RUnlock()

	// 缓存过期：挑一个健康账号拉
	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	ids, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(ids) == 0 {
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = ids
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	return ids
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 响应记录（日志用）
	lw := &loggedWriter{ResponseWriter: w}

	var lastUID string
	var respTokens int

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		lastUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			_ = acct.SaveAtomic()
		}

		rc, status, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastErr = terr
			h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			kind := upstream.Classify(status, string(h.cfg.Upstream.LastBody))
			switch kind {
			case upstream.ErrHardCredit:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "session dead")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			case upstream.ErrNotFound:
				// 404 短冷却不累计 errCount（防雪崩）
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			default:
				// 轮转下一个账号，不直接返回（防雪崩）
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			}
		}
		defer rc.Close()
		h.cfg.Pool.NoteSuccess(acct.UID)
		if peek.Stream {
			_ = upstream.Stream(lw, rc)
			h.logRequest(r, peek.Model, lastUID, lw.status, start, peek.Stream, 0, upstream.ErrNone, "")
			return
		}
		resp, err := upstream.Aggregate(rc)
		if err != nil {
			writeOpenAIError(lw, http.StatusBadGateway, "upstream_parse", err.Error())
			h.logRequest(r, peek.Model, lastUID, lw.status, start, peek.Stream, 0, upstream.ErrClient, err.Error())
			return
		}
		if usage, ok := resp["usage"].(map[string]any); ok {
			if tv, ok := usage["total_tokens"].(float64); ok {
				respTokens = int(tv)
			}
		}
		writeJSON(lw, http.StatusOK, resp)
		h.logRequest(r, peek.Model, lastUID, lw.status, start, peek.Stream, respTokens, upstream.ErrNone, "")
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	kind := upstream.ErrNone
	if lastErr != nil {
		msg += ": " + lastErr.Error()
		if ue, ok := lastErr.(*upstream.Error); ok {
			kind = ue.Kind
		}
	}
	writeOpenAIError(lw, http.StatusServiceUnavailable, "no_healthy_account", msg)
	h.logRequest(r, peek.Model, lastUID, lw.status, start, peek.Stream, 0, kind, msg)
}

// loggedWriter 捕获写给客户端的状态码。
type loggedWriter struct {
	http.ResponseWriter
	status int
}

func (l *loggedWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

func (l *loggedWriter) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logRequest 记录一条请求日志。
func (h *Handler) logRequest(r *http.Request, model, uid string, status int, start time.Time, stream bool, tokens int, kind upstream.ErrKind, errMsg string) {
	if h.cfg.ReqLog == nil {
		return
	}
	errStr := ""
	if kind != upstream.ErrNone {
		errStr = kind.String()
	}
	if errMsg != "" {
		if errStr != "" {
			errStr += ": "
		}
		// 截断，避免日志爆炸
		if len(errMsg) > 160 {
			errMsg = errMsg[:160]
		}
		errStr += errMsg
	}
	h.cfg.ReqLog.Log(&reqlog.Entry{
		Time:    start,
		Method:  r.Method,
		Path:    r.URL.Path,
		Model:   model,
		UID:     uid,
		Status:  status,
		Duration: float64(time.Since(start).Microseconds()) / 1000.0,
		Stream:  stream,
		RespTokens: tokens,
		ClientIP: clientIP(r),
		Error:   errStr,
	})
}

func clientIP(r *http.Request) string {
	// 反代场景优先 X-Forwarded-For 第一段
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return xff[:i]
		}
		return xff
	}
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
