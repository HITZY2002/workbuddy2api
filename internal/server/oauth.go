package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// WorkBuddy OAuth（与 cmd/login 一致，CN）
const (
	wbUpstreamBase    = "https://copilot.tencent.com"
	wbClientUA        = "CLI/2.63.2 CodeBuddy/2.63.2"
	wbOriginReferer   = "https://www.codebuddy.cn"
	wbEndpointState   = wbUpstreamBase + "/v2/plugin/auth/state?platform=CLI"
	wbEndpointToken   = wbUpstreamBase + "/v2/plugin/auth/token?state="
	wbEndpointAccount = wbUpstreamBase + "/v2/plugin/login/account?state="
	oauthSessionTTL   = 15 * time.Minute
)

type oauthSession struct {
	ID        string
	State     string // workbuddy server-side state
	AuthURL   string
	CreatedAt time.Time
}

type oauthStore struct {
	mu   sync.Mutex
	byID map[string]*oauthSession
}

func newOAuthStore() *oauthStore {
	return &oauthStore{byID: map[string]*oauthSession{}}
}

func (s *oauthStore) put(sess *oauthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.byID[sess.ID] = sess
}

func (s *oauthStore) get(id string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	return s.byID[id]
}

func (s *oauthStore) del(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *oauthStore) gcLocked() {
	now := time.Now()
	for id, sess := range s.byID {
		if now.Sub(sess.CreatedAt) > oauthSessionTTL {
			delete(s.byID, id)
		}
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func wbCommonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", wbOriginReferer)
	req.Header.Set("Referer", wbOriginReferer+"/")
	req.Header.Set("User-Agent", wbClientUA)
}

type wbEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func wbDoJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		wbCommonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env wbEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// adminOAuthStart 发起 WorkBuddy OAuth，返回浏览器授权 URL。
func (h *Handler) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	data, _, err := wbDoJSON(client, http.MethodPost, wbEndpointState, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": "发起授权失败: " + err.Error()})
		return
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": "上游未返回 state/authUrl"})
		return
	}
	id := newSessionID()
	h.oauth.put(&oauthSession{
		ID:        id,
		State:     st.State,
		AuthURL:   st.AuthURL,
		CreatedAt: time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": id,
		"auth_url":   st.AuthURL,
		"expires_in": int(oauthSessionTTL.Seconds()),
		"message":    "请在浏览器打开授权链接，完成后点「我已授权」或等待自动检测",
	})
}

// adminOAuthPoll 轮询授权结果；成功则落盘并热加载到 pool。
func (h *Handler) adminOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if req.SessionID == "" {
		// also accept query
		req.SessionID = r.URL.Query().Get("session_id")
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "status": "error", "message": "session_id required"})
		return
	}
	sess := h.oauth.get(req.SessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "status": "error", "message": "会话不存在或已过期，请重新发起授权"})
		return
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	tokRaw, status, errTok := wbDoJSON(client, http.MethodGet, wbEndpointToken+sess.State, nil, nil)
	if errTok != nil {
		// pending: business code non-zero or 4xx waiting
		if status > 0 && status < 500 {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "status": "pending", "message": "等待浏览器完成登录…",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": "error", "message": "token 查询失败: " + errTok.Error(),
		})
		return
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "pending", "message": "等待浏览器完成登录…",
		})
		return
	}

	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(req *http.Request) {
		wbCommonHeaders(req)
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := wbDoJSON(client, http.MethodGet, wbEndpointAccount+sess.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	if acct.UID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": "error", "message": "已拿到 token 但缺少 uid",
		})
		return
	}

	expiresAt := time.Now().Unix() + tok.ExpiresIn
	if tok.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(30 * 24 * time.Hour).Unix()
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "status": "error", "message": "创建 auth_dir 失败: " + err.Error(),
		})
		return
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		FilePath:     filepath.Join(h.cfg.AuthDir, "workbuddy-"+acct.UID+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "status": "error", "message": "落盘失败: " + err.Error(),
		})
		return
	}
	h.cfg.Pool.Add(a)
	// 非阻塞尝试签到 + 刷积分
	go func(authCopy *auth.Auth) {
		_ = h.cfg.Upstream.DailyCheckin(authCopy)
		if remain, err := h.cfg.Upstream.UserResource(authCopy); err == nil {
			h.cfg.Pool.ReenableIfCredits(authCopy.UID, remain)
		}
	}(a)

	h.oauth.del(req.SessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": "done",
		"message": fmt.Sprintf("登录成功：%s (%s)", nonempty(acct.Nickname, "未命名"), shortID(acct.UID)),
		"account": map[string]any{
			"uid":      acct.UID,
			"nickname": acct.Nickname,
			"domain":   tok.Domain,
		},
	})
}

func nonempty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func shortID(u string) string {
	if len(u) <= 12 {
		return u
	}
	return u[:8] + "…"
}
