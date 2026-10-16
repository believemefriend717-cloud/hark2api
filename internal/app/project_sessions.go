package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type projectSession struct {
	MappingID      string    `json:"mapping_id"`
	AccountID      string    `json:"account_id"`
	ConversationID string    `json:"conversation_id"`
	LastUsed       time.Time `json:"last_used"`
	Ending         bool      `json:"ending,omitempty"`
	Archived       bool      `json:"archived,omitempty"`
	Busy           bool      `json:"-"`
}

func validateSession(v chatRequest) error {
	if len(v.SessionID) > 256 || strings.TrimSpace(v.SessionID) != v.SessionID || strings.IndexFunc(v.SessionID, unicode.IsControl) >= 0 {
		return inputError("invalid_session", "会话 ID 需要不超过 256 字节，且不含首尾空格或控制字符")
	}
	if v.SessionEnd && v.SessionID == "" {
		return inputError("invalid_session", "结束会话需要提供 session_id 或 X-Hark-Session-Id")
	}
	return nil
}

func projectMapping(r *http.Request, sessionID string) string {
	namespace := "admin-playground"
	if r.URL.Path == "/v1/chat/completions" {
		h := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
		namespace = "api:" + hex.EncodeToString(h[:])
	}
	if sessionID == "" {
		return randomID("ephemeral_")
	}
	h := sha256.Sum256([]byte(namespace + "\x00" + sessionID))
	return hex.EncodeToString(h[:])
}

// A Project is pinned to its original account. Never fail over a live session
// to another account or send concurrent messages into the same Project.
func (s *Server) acquireProject(r *http.Request, v chatRequest) (*projectSession, Account, bool, error) {
	id := projectMapping(r, v.SessionID)
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	p := s.projects[id]
	if p != nil {
		if p.Busy {
			return nil, Account{}, false, &upstreamError{409, "session_busy", "同一会话正在处理请求，请等待完成后再发送"}
		}
		if p.Ending && !p.Archived {
			return nil, Account{}, false, &upstreamError{409, "session_ending", "会话正在归档，请稍后再开启新会话"}
		}
	}
	if p != nil && p.Archived {
		p = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var account Account
	for _, a := range s.accounts {
		if a.Status == "valid" && (p == nil || a.ID == p.AccountID) {
			account = a
			break
		}
	}
	if account.ID == "" {
		return nil, Account{}, false, &upstreamError{409, "no_valid_account", "请先导入 Cookie 并检测原会话账号有效"}
	}
	reused := p != nil && p.ConversationID != ""
	if p == nil {
		p = &projectSession{MappingID: id, AccountID: account.ID}
		s.projects[id] = p
	}
	p.Busy, p.LastUsed = true, time.Now().UTC()
	return p, account, reused, nil
}

func (s *Server) releaseProject(p *projectSession) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	p.Busy, p.LastUsed = false, time.Now().UTC()
	if p.ConversationID == "" || p.Archived {
		delete(s.projects, p.MappingID)
	}
	if err := s.saveProjectsLocked(); err != nil {
		s.projectWarning("会话映射保存失败")
	}
}

func (s *Server) projectWarning(detail string) {
	s.mu.Lock()
	s.event("warning", "Project 会话", detail)
	s.mu.Unlock()
}

func (s *Server) persistProjects() error {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if err := s.saveProjectsLocked(); err != nil {
		s.projectWarning("会话映射保存失败")
		return &upstreamError{500, "session_store_failed", "Project 会话映射保存失败；未自动重发消息"}
	}
	return nil
}

func (s *Server) saveProjectsLocked() error {
	items := make(map[string]*projectSession)
	for id, p := range s.projects {
		if p.ConversationID != "" {
			items[id] = p
		}
	}
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return atomicPrivate(filepath.Join(s.data, "project-sessions.json"), b)
}

func (s *Server) loadProjects() error {
	s.projects = make(map[string]*projectSession)
	b, err := os.ReadFile(filepath.Join(s.data, "project-sessions.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(b) > 16*1024*1024 {
		return errors.New("project session store unreadable or too large")
	}
	if json.Unmarshal(b, &s.projects) != nil || s.projects == nil {
		return errors.New("project session store invalid")
	}
	for id, p := range s.projects {
		if p == nil || p.MappingID != id || p.AccountID == "" || p.ConversationID == "" || len(p.ConversationID) > 200 || p.LastUsed.IsZero() {
			return errors.New("project session record invalid")
		}
	}
	return nil
}

// Archiving uses a separate bounded context so a disconnected client cannot
// cancel cleanup after a completed answer. Only this gateway's Projects enter
// this function, and archive failure keeps their mapping for retry.
func (s *Server) archiveProject(p *projectSession, cookie string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.harkJSON(ctx, http.MethodPost, "/api/conversations/"+url.PathEscape(p.ConversationID)+"/archive", cookie, struct{}{}, nil); err != nil {
		s.projectWarning("Project 归档失败，保留映射并等待重试")
		return false
	}
	s.projectMu.Lock()
	p.Archived = true
	err := s.saveProjectsLocked()
	s.projectMu.Unlock()
	if err != nil {
		s.projectWarning("Project 已归档，归档状态保存失败")
	}
	return true
}

func (s *Server) cleanupProjects(now time.Time, idle time.Duration) {
	s.projectMu.Lock()
	var pending []*projectSession
	for _, p := range s.projects {
		if !p.Busy && (p.Ending || p.Archived || now.Sub(p.LastUsed) >= idle) {
			p.Busy = true
			pending = append(pending, p)
		}
	}
	s.projectMu.Unlock()
	for _, p := range pending {
		s.mu.Lock()
		cookie := ""
		for _, a := range s.accounts {
			if a.ID == p.AccountID {
				cookie = a.Cookie
				break
			}
		}
		s.mu.Unlock()
		if !p.Archived && cookie != "" {
			s.archiveProject(p, cookie)
		}
		s.projectMu.Lock()
		p.Busy = false
		if p.Archived {
			delete(s.projects, p.MappingID)
		}
		if err := s.saveProjectsLocked(); err != nil {
			s.projectWarning("归档清理状态保存失败")
		}
		s.projectMu.Unlock()
	}
}

func (s *Server) RunProjectCleanup(ctx context.Context, idle time.Duration) {
	s.cleanupProjects(time.Now(), idle)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.cleanupProjects(now, idle)
		}
	}
}
