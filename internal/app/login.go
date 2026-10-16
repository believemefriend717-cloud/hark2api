package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const harkLoginURL = "https://hark.com/login"
const harkSessionURL = "https://hark.com/api/auth/get-session"
const loginLifetime = 15 * time.Minute

// This local one-use capability is separate from Hark's browser-owned OAuth state.
type loginAttempt struct {
	mu              sync.Mutex
	id, name, token string
	expires         time.Time
	done, delivered bool
	account         *Account
	cookie          string
}

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if len([]rune(input.Name)) > 40 {
		fail(w, 400, "invalid_name", "名称最多 40 个字符")
		return
	}
	s.loginMu.Lock()
	if len(s.logins) >= 4 {
		s.loginMu.Unlock()
		fail(w, 409, "login_limit", "请完成或取消已有导入请求")
		return
	}
	a := &loginAttempt{id: randomID("login_"), name: input.Name, token: randomID("bridge_"), expires: time.Now().Add(loginLifetime)}
	s.logins[a.id] = a
	s.loginMu.Unlock()
	time.AfterFunc(loginLifetime, func() { s.removeLogin(a.id) })
	write(w, 201, map[string]any{"id": a.id, "bridge_token": a.token, "auth_url": harkLoginURL, "expires_at": a.expires.UTC().Format(time.RFC3339), "mode": "browser_extension"})
}

func (s *Server) attempt(id string) *loginAttempt {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	return s.logins[id]
}
func (s *Server) removeLogin(id string) {
	s.loginMu.Lock()
	a := s.logins[id]
	delete(s.logins, id)
	s.loginMu.Unlock()
	if a != nil {
		a.mu.Lock()
		a.done = true
		a.token = ""
		a.cookie = ""
		a.mu.Unlock()
	}
}

func (s *Server) loginStatus(w http.ResponseWriter, r *http.Request) {
	a := s.attempt(r.URL.Query().Get("id"))
	if a == nil {
		fail(w, 410, "login_expired", "导入请求已结束，请重新生成")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Now().After(a.expires) {
		fail(w, 410, "login_expired", "导入请求已过期")
		return
	}
	status := "pending"
	if a.account != nil {
		status = "complete"
	}
	write(w, 200, map[string]any{"status": status})
}

// Compatibility endpoint; never launches a browser process.
func (s *Server) loginOpen(w http.ResponseWriter, r *http.Request) {
	fail(w, 409, "extension_required", "请刷新管理页并启用 Hark 账号导入扩展")
}

func (s *Server) loginComplete(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &input) {
		return
	}
	a := s.attempt(input.ID)
	if a == nil {
		fail(w, 410, "login_expired", "导入请求已结束")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Now().After(a.expires) || a.delivered {
		fail(w, 410, "login_expired", "导入请求已结束")
		return
	}
	if a.account == nil {
		fail(w, 409, "login_pending", "等待扩展回传 Hark 登录会话，请在授权窗口完成登录")
		return
	}
	a.delivered = true
	write(w, 201, map[string]any{"account": a.account, "cookie": a.cookie, "message": "Cookie 已验证并导入"})
	a.cookie = ""
}

func (s *Server) loginCancel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &input) {
		return
	}
	s.removeLogin(input.ID)
	write(w, 200, map[string]bool{"ok": true})
}

// Only this route accepts extension callbacks. A high-entropy capability issued
// by a same-origin admin request is checked, expired and consumed atomically.
// Cookie values never appear in URLs, logs or status responses.
func (s *Server) loginBridge(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID     string `json:"id"`
		Token  string `json:"token"`
		Cookie string `json:"cookie"`
	}
	if !decode(w, r, &input) {
		return
	}
	a := s.attempt(input.ID)
	if a == nil {
		fail(w, 410, "login_expired", "导入请求已结束")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done || time.Now().After(a.expires) {
		fail(w, 410, "login_expired", "导入请求已结束")
		return
	}
	if len(input.Token) != len(a.token) || subtle.ConstantTimeCompare([]byte(input.Token), []byte(a.token)) != 1 {
		fail(w, 403, "invalid_bridge_token", "导入标识不匹配")
		return
	}
	cookie := strings.TrimSpace(input.Cookie)
	if !strings.Contains(cookie, "=") || strings.ContainsAny(cookie, "\r\n") || len(cookie) > 16000 {
		fail(w, 400, "invalid_cookie", "登录会话格式不正确")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, harkSessionURL, nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		fail(w, 502, "session_check_failed", "Hark 会话检测连接失败，请重试")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail(w, 502, "session_check_failed", fmt.Sprintf("Hark 会话检测返回 HTTP %d", resp.StatusCode))
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	var data struct {
		Session *struct {
			ID string `json:"id"`
		} `json:"session"`
		User *struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err != nil || len(b) > 65536 || json.Unmarshal(b, &data) != nil || data.Session == nil || data.Session.ID == "" || data.User == nil || data.User.ID == "" {
		fail(w, 409, "login_pending", "Hark 尚未返回有效登录会话")
		return
	}
	name := a.name
	if name == "" {
		name = data.User.Name
	}
	if name == "" {
		name = data.User.Email
	}
	if name == "" {
		name = "Hark 账号"
	}
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}
	account := Account{ID: randomID("acct_"), Name: name, Status: "valid", Checked: time.Now().UTC().Format(time.RFC3339), Detail: "已通过浏览器扩展导入并验证 Cookie；聊天链路仍待接入", Cookie: cookie}
	s.mu.Lock()
	s.accounts = append(s.accounts, account)
	if err := s.saveAccounts(); err != nil {
		s.accounts = s.accounts[:len(s.accounts)-1]
		s.mu.Unlock()
		fail(w, 500, "save_failed", "Cookie 保存失败，请重试")
		return
	}
	s.event("success", "导入账号", "浏览器会话已验证并加入账号列表")
	s.mu.Unlock()
	a.account = &account
	a.cookie = cookie
	a.token = ""
	a.done = true
	write(w, 201, map[string]bool{"ok": true})
}
