package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

type Settings struct {
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
	Timeout  int    `json:"timeout"`
}
type Key struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	Created string `json:"created"`
	Hash    string `json:"hash,omitempty"`
}
type Account struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Checked      string `json:"checked,omitempty"`
	Detail       string `json:"detail"`
	Cookie       string `json:"-"`
	ChatVerified bool   `json:"chat_verified"`
	ChatChecked  string `json:"chat_checked,omitempty"`
}
type Event struct {
	Time   string `json:"time"`
	Level  string `json:"level"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}
type persisted struct {
	Settings Settings `json:"settings"`
	Keys     []Key    `json:"keys"`
}
type Server struct {
	publicOrigin  string
	adminHash     []byte
	adminSessions map[string]adminSession
	authAttempts  map[string]authThrottle
	mu            sync.Mutex
	data          string
	settings      Settings
	keys          []Key
	accounts      []Account
	events        []Event
	started       time.Time
	client        *http.Client
	upstreamProxy string
	loginMu       sync.Mutex
	logins        map[string]*loginAttempt
	projectMu     sync.Mutex
	projects      map[string]*projectSession
}

// SetUpstreamProxy applies one explicit HTTP proxy to all outbound Hark calls.
// Empty keeps the direct route, which preserves the library default for tests.
func (s *Server) SetUpstreamProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if raw == "" {
		transport.Proxy = nil
		s.client.Transport = transport
		s.upstreamProxy = ""
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("upstream proxy must be an HTTP URL without credentials or path")
	}
	transport.Proxy = http.ProxyURL(u)
	s.client.Transport = transport
	s.upstreamProxy = raw
	return nil
}

func New(data string) (*Server, error) {
	s := &Server{data: data, settings: Settings{Name: "Hark Gateway", Timezone: "Asia/Shanghai", Timeout: 180},
		keys: []Key{}, accounts: []Account{}, events: []Event{}, started: time.Now(),
		client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
	s.logins = make(map[string]*loginAttempt)
	if err := s.configureAccess(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(data, "config.json"))
	if err == nil {
		var p persisted
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err = validSettings(p.Settings); err != nil {
			return nil, err
		}
		s.settings = p.Settings
		if p.Keys != nil {
			s.keys = p.Keys
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := s.loadAccounts(); err != nil {
		return nil, err
	}
	if err := s.loadProjects(); err != nil {
		return nil, err
	}
	s.event("info", "服务启动", "本地管理界面已就绪")
	return s, nil
}

func randomID(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func (s *Server) event(level, action, detail string) {
	s.events = append(s.events, Event{Time: time.Now().UTC().Format(time.RFC3339), Level: level, Action: action, Detail: detail})
	if len(s.events) > 200 {
		s.events = s.events[len(s.events)-200:]
	}
}

// Caller holds mu. Replace the file only after a complete successful write.
func (s *Server) save() error {
	b, err := json.MarshalIndent(persisted{Settings: s.settings, Keys: s.keys}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.data, ".config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(s.data, "config.json"))
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 32768)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid_input", "输入格式不正确或内容过长")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, 400, "invalid_input", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /admin/auth/login", s.adminLogin)
	m.HandleFunc("POST /admin/auth/logout", s.adminLogout)
	m.HandleFunc("GET /admin/state", s.state)
	m.HandleFunc("POST /admin/keys", s.createKey)
	m.HandleFunc("DELETE /admin/keys/{id}", s.deleteKey)
	m.HandleFunc("POST /admin/accounts", s.addAccount)
	m.HandleFunc("DELETE /admin/accounts/{id}", s.deleteAccount)
	m.HandleFunc("POST /admin/accounts/{id}/check", s.checkAccount)
	m.HandleFunc("POST /admin/accounts/{id}/chat-probe", s.chatProbe)
	m.HandleFunc("POST /admin/login/start", s.loginStart)
	m.HandleFunc("POST /admin/login/open", s.loginOpen)
	m.HandleFunc("POST /admin/login/complete", s.loginComplete)
	m.HandleFunc("POST /admin/login/cancel", s.loginCancel)
	m.HandleFunc("GET /admin/login/status", s.loginStatus)
	m.HandleFunc("POST /bridge/login", s.loginBridge)
	m.HandleFunc("PUT /admin/settings", s.updateSettings)
	m.HandleFunc("POST /admin/validate", s.validate)
	m.HandleFunc("POST /admin/playground/chat", s.playgroundChat)
	m.HandleFunc("GET /v1/models", s.models)
	m.HandleFunc("POST /v1/chat/completions", s.chat)
	files, _ := fs.Sub(assets, "web")
	m.Handle("GET /", http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		host := r.Host
		if !s.allowedHost(host) {
			fail(w, 403, "local_only", "管理界面仅供本机访问")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
				fail(w, 403, "invalid_origin", "请求来源不正确")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				if origin != requestOrigin(r) {
					fail(w, 403, "invalid_origin", "请求来源不正确")
					return
				}
			}
			if r.Method != "GET" && r.Header.Get("X-Hark-Admin") != "1" {
				fail(w, 403, "invalid_origin", "缺少本地管理请求标记")
				return
			}
			if r.URL.Path != "/admin/auth/login" && !s.isAdmin(r) {
				fail(w, 401, "admin_auth_required", "请先使用管理员密钥登录")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]Key, len(s.keys))
	copy(keys, s.keys)
	for i := range keys {
		keys[i].Hash = ""
	}
	connected := false
	for _, a := range s.accounts {
		if a.Status == "valid" && a.ChatVerified {
			connected = true
		}
	}
	providerStatus := "Project 聊天待本账号实测"
	if connected {
		providerStatus = "Project 聊天已实测通过"
	}
	write(w, 200, map[string]any{"settings": s.settings, "keys": keys, "accounts": s.accounts, "events": s.events,
		"endpoint": requestOrigin(r) + "/v1", "started": s.started.UTC().Format(time.RFC3339), "upstream_proxy": s.upstreamProxy,
		"provider": map[string]any{"name": "Hark", "model": "hark", "connected": connected, "status": providerStatus, "mode": "project", "project_protocol": "verified", "context_isolation": "instruction-only", "hard_isolation": false}})
}
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len([]rune(input.Name)) > 40 {
		fail(w, 400, "invalid_name", "名称需要 1–40 个字符")
		return
	}
	plain := randomID("sk-hark-")
	h := sha256.Sum256([]byte(plain))
	k := Key{ID: randomID("key_"), Name: input.Name, Prefix: plain[:16], Hash: hex.EncodeToString(h[:]), Created: time.Now().UTC().Format(time.RFC3339)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	if err := s.save(); err != nil {
		s.keys = s.keys[:len(s.keys)-1]
		fail(w, 500, "save_failed", "保存失败，请检查数据目录")
		return
	}
	s.event("success", "创建 API Key", "已创建一个客户端 Key")
	k.Hash = ""
	write(w, 201, map[string]any{"key": k, "secret": plain})
}
func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keys {
		if k.ID == r.PathValue("id") {
			old := s.keys
			s.keys = append(append([]Key{}, old[:i]...), old[i+1:]...)
			if err := s.save(); err != nil {
				s.keys = old
				fail(w, 500, "save_failed", "保存失败")
				return
			}
			s.event("info", "删除 API Key", "客户端 Key 已撤销")
			write(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 404, "not_found", "Key 不存在")
}
func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Cookie = strings.TrimSpace(input.Cookie)
	if input.Name == "" || len([]rune(input.Name)) > 40 {
		fail(w, 400, "invalid_name", "名称需要 1–40 个字符")
		return
	}
	if !strings.Contains(input.Cookie, "=") || strings.ContainsAny(input.Cookie, "\r\n") || len(input.Cookie) > 16000 {
		fail(w, 400, "invalid_cookie", "请输入单行 Cookie，格式为 name=value; name=value")
		return
	}
	a := Account{ID: randomID("acct_"), Name: input.Name, Status: "unchecked", Detail: "尚未检测会话", Cookie: input.Cookie}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = append(s.accounts, a)
	if err := s.saveAccounts(); err != nil {
		s.accounts = s.accounts[:len(s.accounts)-1]
		fail(w, 500, "save_failed", "Cookie 保存失败，请检查数据目录")
		return
	}
	s.event("info", "导入账号", "Cookie 已加密保存")
	write(w, 201, a)
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if a.ID == r.PathValue("id") {
			old := s.accounts
			s.accounts = append(append([]Account{}, old[:i]...), old[i+1:]...)
			if err := s.saveAccounts(); err != nil {
				s.accounts = old
				fail(w, 500, "save_failed", "账号移除保存失败")
				return
			}
			s.event("info", "移除账号", "本地会话已移除")
			write(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 404, "not_found", "账号不存在")
}
func (s *Server) checkAccount(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var account Account
	for _, a := range s.accounts {
		if a.ID == r.PathValue("id") {
			account = a
			break
		}
	}
	s.mu.Unlock()
	if account.ID == "" {
		fail(w, 404, "not_found", "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://hark.com/api/auth/get-session", nil)
	req.Header.Set("Cookie", account.Cookie)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	status, detail := "error", "检测失败，请稍后重试"
	if err == nil {
		defer resp.Body.Close()
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 65537))
		var body struct {
			Session *struct {
				ID string `json:"id"`
			} `json:"session"`
			User *struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		switch {
		case resp.StatusCode == 403:
			status, detail = "blocked", "Hark 返回 HTTP 403，访问受阻"
		case resp.StatusCode == 401:
			status, detail = "expired", "登录会话已失效"
		case resp.StatusCode == 429:
			status, detail = "limited", "检测受到限流，请稍后再试"
		case resp.StatusCode != 200:
			detail = fmt.Sprintf("Hark 返回 HTTP %d", resp.StatusCode)
		case readErr != nil || len(b) > 65536:
			detail = "会话响应读取失败"
		case bytes.Equal(bytes.TrimSpace(b), []byte("null")):
			status, detail = "expired", "未检测到有效登录会话"
		case json.Unmarshal(b, &body) == nil && body.Session != nil && body.Session.ID != "" && body.User != nil && body.User.ID != "":
			status, detail = "valid", "登录会话有效；Project 聊天待本账号实测"
			if account.ChatVerified {
				detail = "登录会话有效；Project 聊天已实测通过"
			}
		default:
			detail = "会话响应结构尚未确认"
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == account.ID {
			old := s.accounts[i]
			s.accounts[i].Status = status
			s.accounts[i].Detail = detail
			s.accounts[i].Checked = time.Now().UTC().Format(time.RFC3339)
			if err := s.saveAccounts(); err != nil {
				s.accounts[i] = old
				fail(w, 500, "save_failed", "会话状态保存失败")
				return
			}
			level := "info"
			if status == "valid" {
				level = "success"
			} else {
				level = "warning"
			}
			s.event(level, "检测会话", detail)
			write(w, 200, s.accounts[i])
			return
		}
	}
	fail(w, 404, "not_found", "账号已移除")
}
func validSettings(v Settings) error {
	if strings.TrimSpace(v.Name) == "" || len([]rune(v.Name)) > 40 {
		return errors.New("名称需要 1–40 个字符")
	}
	if v.Timeout < 30 || v.Timeout > 600 {
		return errors.New("超时需要在 30–600 秒之间")
	}
	allowed := map[string]bool{"Asia/Shanghai": true, "UTC": true, "Asia/Tokyo": true, "America/New_York": true, "Europe/London": true}
	if !allowed[v.Timezone] {
		return errors.New("请选择有效的时区")
	}
	return nil
}
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var v Settings
	if !decode(w, r, &v) {
		return
	}
	v.Name = strings.TrimSpace(v.Name)
	if err := validSettings(v); err != nil {
		fail(w, 400, "invalid_settings", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.settings
	s.settings = v
	if err := s.save(); err != nil {
		s.settings = old
		fail(w, 500, "save_failed", "保存失败")
		return
	}
	s.event("success", "更新配置", "本地配置已保存")
	write(w, 200, v)
}

type chatRequest struct {
	Model        string          `json:"model"`
	Messages     []chatMessage   `json:"messages"`
	Stream       bool            `json:"stream"`
	SessionID    string          `json:"session_id,omitempty"`
	SessionEnd   bool            `json:"session_end,omitempty"`
	OriginalBody json.RawMessage `json:"-"`
}

func validateChat(v chatRequest) error {
	if v.Model != "hark" {
		return errors.New("当前模型别名为 hark")
	}
	if len(v.Messages) == 0 || len(v.Messages) > 100 {
		return errors.New("请输入 1–100 条文本消息")
	}
	for _, m := range v.Messages {
		if m.Role == "user" && strings.TrimSpace(m.Content) == "" {
			return errors.New("user 消息内容不能为空")
		}
	}
	return nil
}
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var v chatRequest
	if !decodeChat(w, r, &v) {
		return
	}
	if err := validateChat(v); err != nil {
		fail(w, 400, "invalid_request", err.Error())
		return
	}
	s.mu.Lock()
	s.event("success", "校验请求", "文本请求格式通过；未发送到 Hark")
	s.mu.Unlock()
	write(w, 200, map[string]any{"valid": true, "upstream_sent": false, "message": "请求格式正确；此处仅校验，请使用 API Key 调用聊天接口。"})
}

// The dashboard is already protected by the administrator session. Keeping
// this relay server-side means the browser never receives or submits an API
// Key; the stored Hark account remains the only upstream credential.
func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request) {
	var v chatRequest
	if !decodeChat(w, r, &v) {
		return
	}
	if err := validateChat(v); err != nil {
		fail(w, 400, "invalid_request", err.Error())
		return
	}
	s.directChat(w, r, v)
}
func (s *Server) authorized(r *http.Request) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if value == "" || value == r.Header.Get("Authorization") {
		return false
	}
	h := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(h[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.Hash == hash {
			return true
		}
	}
	return false
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		fail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	write(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": "hark", "object": "model", "owned_by": "hark", "created": 0}}})
}
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		fail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	var v chatRequest
	if !decodeChat(w, r, &v) {
		return
	}
	if err := validateChat(v); err != nil {
		fail(w, 400, "invalid_request", err.Error())
		return
	}
	s.directChat(w, r, v)
}
