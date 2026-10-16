package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type upstreamError struct {
	status        int
	code, message string
}

func (e *upstreamError) Error() string { return e.message }

func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Server) harkJSON(ctx context.Context, method, path, cookie string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://hark.com"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://hark.com")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return &upstreamError{502, "upstream_connection_failed", "Hark 连接失败"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		var rejection struct {
			Error string `json:"error"`
		}
		if resp.StatusCode == 403 && json.Unmarshal(body, &rejection) == nil && rejection.Error == "run override not enabled" {
			return &upstreamError{403, "upstream_run_override_denied", "Hark: run override not enabled"}
		}
		code, message := "upstream_http_error", fmt.Sprintf("Hark 返回 HTTP %d", resp.StatusCode)
		status := 502
		switch resp.StatusCode {
		case 400, 422:
			status, code, message = resp.StatusCode, "upstream_request_rejected", fmt.Sprintf("Hark 拒绝转发的请求参数（HTTP %d）", resp.StatusCode)
		case 401:
			code, message = "upstream_session_expired", "Hark 登录会话已失效，请重新导入 Cookie"
		case 403:
			code, message = "upstream_access_blocked", "Hark 返回 HTTP 403，访问受阻"
		case 429:
			code, message = "upstream_rate_limited", "Hark 请求限流，请稍后再试"
		}
		return &upstreamError{status, code, message}
	}
	if output == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(b) > 2*1024*1024 {
		return &upstreamError{502, "upstream_response_invalid", "Hark 响应读取失败或内容过长"}
	}
	if json.Unmarshal(b, output) != nil {
		return &upstreamError{502, "upstream_response_invalid", "Hark 返回非预期 JSON 响应"}
	}
	return nil
}

type harkMessage struct {
	ID           string          `json:"id"`
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	Status       string          `json:"status"`
	State        string          `json:"state"`
	JobStatus    string          `json:"jobStatus"`
	Streaming    *bool           `json:"isStreaming"`
	TriggeredBy  string          `json:"triggeredByMessageId"`
	Source       string          `json:"source"`
	Error        json.RawMessage `json:"error"`
	Presentation struct {
		Kind string `json:"kind"`
	} `json:"presentation"`
	Onboarding json.RawMessage `json:"onboarding"`
}

// Hark uses responseMessageId for a job record, and separate send_bubble IDs
// for text. Correlate both through the submitted user message ID and wait for
// all observed jobs/bubbles in that request to finish, never for stable text.
type projectReply struct {
	Text    string
	Outcome string
}

func completedReply(messages []harkMessage, expected, requestID string) (string, error) {
	reply, err := completedProjectReply(messages, expected, requestID)
	return reply.Text, err
}

func completedProjectReply(messages []harkMessage, expected, requestID string) (projectReply, error) {
	primaryDone, pending := false, false
	var parts []string
	for _, m := range messages {
		if (m.ID != expected && (requestID == "" || m.TriggeredBy != requestID)) || m.Role != "assistant" || len(m.Onboarding) > 0 && string(m.Onboarding) != "null" {
			continue
		}
		if m.ID == expected && requestID != "" && m.TriggeredBy != requestID {
			continue
		}
		status := m.JobStatus
		if status == "" {
			status = m.Status
		}
		if status == "" {
			status = m.State
		}
		if len(m.Error) > 0 && string(m.Error) != "null" && string(m.Error) != "false" && string(m.Error) != `""` {
			return projectReply{}, &upstreamError{502, "upstream_generation_failed", "Hark 回复任务返回错误"}
		}
		switch status {
		case "failed", "error", "cancelled", "canceled", "interrupted", "cost_capped":
			return projectReply{}, &upstreamError{502, "upstream_generation_failed", "Hark 回复任务失败或取消"}
		case "completed", "complete", "finished", "done":
			if m.Streaming != nil && *m.Streaming {
				pending = true
			} else if m.ID == expected {
				primaryDone = true
			}
		default:
			if status != "" || m.ID == expected {
				pending = true
			}
		}
		if m.Streaming != nil && *m.Streaming {
			pending = true
		}
		if strings.TrimSpace(m.Content) != "" && (m.ID == expected || m.Source == "send_bubble" && (m.Presentation.Kind == "text" || m.Presentation.Kind == "summary")) {
			if m.Streaming == nil && status == "" {
				return projectReply{}, &upstreamError{502, "upstream_protocol_unverified", "Hark 文字回复缺少已识别的完成字段"}
			}
			if m.Streaming != nil && !*m.Streaming || status == "completed" || status == "complete" || status == "finished" || status == "done" {
				parts = append(parts, m.Content)
			}
		}
	}
	if primaryDone && !pending {
		if len(parts) > 0 {
			return projectReply{Text: strings.Join(parts, "\n\n"), Outcome: "completed"}, nil
		}
		if notice := declinedNoticeForTurn(messages, expected, requestID); notice != "" {
			return projectReply{Text: notice, Outcome: "declined"}, nil
		}
	}
	return projectReply{}, nil
}

// Hark can append a declined_notice without triggeredByMessageId after an
// empty completed job. Bind it by history order inside the submitted user turn,
// never by timestamp or by the notice's text. The caller checks all bound jobs.
func declinedNoticeForTurn(messages []harkMessage, expected, requestID string) string {
	if expected == "" || requestID == "" {
		return ""
	}
	inTurn, primarySeen := false, false
	for _, m := range messages {
		if m.Role == "user" {
			if inTurn {
				break
			}
			inTurn = m.ID == requestID
			continue
		}
		if !inTurn {
			continue
		}
		if m.ID == expected && m.Role == "assistant" && m.TriggeredBy == requestID {
			primarySeen = true
			continue
		}
		if primarySeen && m.Role == "assistant" && m.Source == "declined_notice" && m.Presentation.Kind == "declined" &&
			(m.TriggeredBy == "" || m.TriggeredBy == requestID) && m.Streaming != nil && !*m.Streaming &&
			(len(m.Onboarding) == 0 || string(m.Onboarding) == "null") && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

func transcript(v chatRequest) string {
	if len(v.Messages) == 1 && v.Messages[0].Role == "user" {
		return v.Messages[0].Content
	}
	if len(v.OriginalBody) > 0 {
		var original map[string]json.RawMessage
		if json.Unmarshal(v.OriginalBody, &original) == nil {
			return string(original["messages"])
		}
	}
	b, _ := json.Marshal(v.Messages)
	return string(b)
}

func (s *Server) directChat(w http.ResponseWriter, r *http.Request, v chatRequest) {
	w.Header().Set("X-Hark-Context-Isolation", "instruction-only")
	s.mu.Lock()
	timeout, zone := s.settings.Timeout, s.settings.Timezone
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeout)*time.Second)
	defer cancel()
	p, account, reused, err := s.acquireProject(r, v)
	if err == nil {
		defer s.releaseProject(p)
		if p.ConversationID == "" {
			var cid string
			cid, err = s.createProject(ctx, account.Cookie, zone)
			if err == nil {
				s.projectMu.Lock()
				p.ConversationID = cid
				s.projectMu.Unlock()
				err = s.persistProjects()
			}
		}
	}
	var reply projectReply
	if err == nil {
		reply, err = s.sendNativeProject(ctx, account.Cookie, zone, p.ConversationID, v, 2*time.Second)
	}
	if err != nil {
		status, code, message := 502, "upstream_error", "Hark 聊天请求失败"
		var e *upstreamError
		if errors.As(err, &e) {
			status, code, message = e.status, e.code, e.message
		}
		if ctx.Err() != nil {
			status, code, message = 504, "upstream_timeout", "等待 Hark 关联且完成的回复超时；请求可能已受理，请勿自动重发"
		}
		s.mu.Lock()
		s.event("warning", "聊天请求", code)
		s.mu.Unlock()
		if r.Context().Err() == nil {
			fail(w, status, code, message)
		}
		return
	}
	id, created := randomID("chatcmpl_"), time.Now().Unix()
	archived := false
	if v.SessionEnd {
		s.projectMu.Lock()
		p.Ending = true
		s.projectMu.Unlock()
		if s.persistProjects() == nil {
			archived = s.archiveProject(p, account.Cookie)
		}
	}
	isolation := "key-session"
	if v.SessionID == "" {
		isolation = "per-request"
	}
	w.Header().Set("X-Hark-Conversation-Isolation", isolation)
	w.Header().Set("X-Hark-Session-Mode", "project")
	w.Header().Set("X-Hark-Project-Reused", strconv.FormatBool(reused))
	w.Header().Set("X-Hark-Project-Archived", strconv.FormatBool(archived))
	w.Header().Set("X-Hark-Upstream-Outcome", reply.Outcome)
	text := reply.Text
	if !v.Stream {
		write(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": "hark", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
	} else {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Hark-Stream-Mode", "buffered")
		for _, choice := range []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": text}, "finish_reason": nil}, map[string]any{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}} {
			b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": "hark", "choices": []any{choice}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if reply.Outcome == "declined" {
		s.event("warning", "聊天请求", "Hark 已完成任务并返回 declined_notice；已透传结束提示")
		return
	}
	for i := range s.accounts {
		if s.accounts[i].ID == account.ID {
			s.accounts[i].ChatVerified = true
			s.accounts[i].ChatChecked = time.Now().UTC().Format(time.RFC3339)
			s.accounts[i].Detail = "登录会话有效；Project 聊天已实测通过"
			if err := s.saveAccounts(); err != nil {
				s.event("warning", "保存聊天状态", "Project 回复成功，验证状态保存失败")
			}
			break
		}
	}
	s.event("success", "聊天请求", "独立 Project 已返回关联且完成的回复")
}

func (s *Server) runDirect(ctx context.Context, cookie, zone string, v chatRequest, interval time.Duration) (string, error) {
	cid, err := s.createProject(ctx, cookie, zone)
	if err != nil {
		return "", err
	}
	return s.sendProject(ctx, cookie, zone, cid, transcript(v), interval)
}

func (s *Server) createProject(ctx context.Context, cookie, zone string) (string, error) {
	var created struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversationId"`
	}
	if err := s.harkJSON(ctx, "POST", "/api/conversations", cookie, map[string]any{"timezone": zone, "title": "Hark2API Project", "autoTitle": true}, &created); err != nil {
		return "", err
	}
	cid := created.ConversationID
	if cid == "" {
		cid = created.ID
	}
	if cid == "" || len(cid) > 200 {
		return "", &upstreamError{502, "upstream_protocol_unverified", "Hark 建会话响应缺少 conversationId"}
	}
	return cid, nil
}

func (s *Server) sendProject(ctx context.Context, cookie, zone, cid, message string, interval time.Duration) (string, error) {
	reply, err := s.sendProjectPayload(ctx, cookie, cid, map[string]any{"timezone": zone, "conversationId": cid, "message": projectScopedMessage(cid, message)}, interval)
	return reply.Text, err
}

func (s *Server) sendNativeProject(ctx context.Context, cookie, zone, cid string, v chatRequest, interval time.Duration) (projectReply, error) {
	payload, err := nativeProjectPayload(v, zone, cid)
	if err != nil {
		return projectReply{}, err
	}
	return s.sendProjectPayload(ctx, cookie, cid, payload, interval)
}

func (s *Server) sendProjectPayload(ctx context.Context, cookie, cid string, payload map[string]any, interval time.Duration) (projectReply, error) {
	expected := uuid()
	requestID := uuid()
	var accepted struct {
		Success        *bool  `json:"success"`
		ConversationID string `json:"conversationId"`
		MessageID      string `json:"messageId"`
		Redirected     *bool  `json:"redirected"`
	}
	payload["idempotencyKey"], payload["responseMessageId"] = requestID, expected
	if err := s.harkJSON(ctx, "POST", "/api/messages/send?cid="+url.QueryEscape(cid), cookie, payload, &accepted); err != nil {
		return projectReply{}, err
	}
	if accepted.Success == nil || !*accepted.Success {
		return projectReply{}, &upstreamError{502, "upstream_protocol_unverified", "Hark 未确认消息受理；未自动重试发送"}
	}
	if accepted.ConversationID != cid || accepted.MessageID != requestID || accepted.Redirected == nil || *accepted.Redirected {
		return projectReply{}, &upstreamError{502, "upstream_project_mismatch", "Hark 未确认消息进入本次独立 Project；未自动重发"}
	}
	for {
		var history struct {
			Messages []harkMessage `json:"messages"`
		}
		if err := s.harkJSON(ctx, "GET", "/api/messages/history?conversationId="+url.QueryEscape(cid), cookie, nil, &history); err != nil {
			return projectReply{}, err
		}
		if reply, err := completedProjectReply(history.Messages, expected, requestID); reply.Text != "" || err != nil {
			return reply, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return projectReply{}, ctx.Err()
		case <-timer.C:
		}
	}
}
