package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxChatBody = 1024 * 1024

type chatInputError struct{ code, message string }

func (e *chatInputError) Error() string     { return e.message }
func inputError(code, message string) error { return &chatInputError{code, message} }

// Validate only the gateway envelope. Original JSON values are retained for
// the native upstream; tool definitions are not rewritten into instructions.
func decodeChat(w http.ResponseWriter, r *http.Request, out *chatRequest) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatBody))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			fail(w, 413, "request_too_large", "聊天请求体超过 1 MiB，请减少历史内容")
		} else {
			fail(w, 400, "invalid_json", "聊天请求体读取失败")
		}
		return false
	}
	v, _, err := normalizeChat(b)
	if err == nil {
		if values, ok := r.Header["X-Hark-Session-Id"]; ok {
			if len(values) != 1 {
				err = inputError("invalid_session", "X-Hark-Session-Id 只能设置一次")
			} else {
				v.SessionID = values[0]
			}
		}
		if values, ok := r.Header["X-Hark-Session-End"]; ok {
			if len(values) != 1 || values[0] != "true" && values[0] != "false" {
				err = inputError("invalid_session", "X-Hark-Session-End 必须是 true 或 false")
			} else {
				v.SessionEnd = values[0] == "true"
			}
		}
		if err == nil {
			err = validateSession(v)
		}
	}
	if err != nil {
		var detail *chatInputError
		if errors.As(err, &detail) {
			fail(w, 400, detail.code, detail.message)
		} else {
			fail(w, 400, "invalid_json", "聊天请求需要一个有效 JSON 对象")
		}
		return false
	}
	w.Header().Set("X-Hark-Input-Adapter", "native-project-v1")
	w.Header().Set("X-Hark-Parameter-Forwarding", "original-json-values")
	*out = v
	return true
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func normalizeChat(b []byte) (chatRequest, []string, error) {
	var v chatRequest
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return v, nil, inputError("invalid_json", "聊天请求需要一个有效 JSON 对象")
	}
	if len(object) > 64 {
		return v, nil, inputError("invalid_request", "聊天请求顶层字段过多")
	}
	if json.Unmarshal(object["model"], &v.Model) != nil || v.Model != "hark" {
		return v, nil, inputError("invalid_request", "当前模型别名为 hark")
	}
	if raw, ok := object["stream"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &v.Stream) != nil {
			return v, nil, inputError("invalid_request", "stream 必须是布尔值")
		}
	}
	if raw, ok := object["session_id"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &v.SessionID) != nil {
			return v, nil, inputError("invalid_session", "session_id 必须是字符串")
		}
	}
	if raw, ok := object["session_end"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &v.SessionEnd) != nil {
			return v, nil, inputError("invalid_session", "session_end 必须是布尔值")
		}
	}
	for _, name := range []string{"conversationId", "idempotencyKey", "responseMessageId", "message"} {
		if _, ok := object[name]; ok {
			return v, nil, inputError("reserved_gateway_field", name+" 由网关管理，请使用 messages 和 session_id")
		}
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(object["messages"], &messages) != nil || len(messages) == 0 || len(messages) > 100 {
		return v, nil, inputError("invalid_request", "messages 需要包含 1–100 条消息对象")
	}
	for _, m := range messages {
		if m.Role == "" {
			return v, nil, inputError("invalid_request", "消息需要 role 字符串")
		}
		text, err := messageText(m.Content)
		if err != nil {
			return v, nil, err
		}
		v.Messages = append(v.Messages, chatMessage{Role: m.Role, Content: text})
	}
	// Do not compile schema, modify roles, trim history, or reject downstream
	// feature options locally. Hark decides whether it supports those options.
	v.OriginalBody = append(json.RawMessage(nil), b...)
	if err := validateChat(v); err != nil {
		return v, nil, inputError("invalid_request", err.Error())
	}
	return v, nil, nil
}

func messageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil && parts != nil {
		var texts []string
		for _, p := range parts {
			if (p.Type != "text" && p.Type != "input_text") || p.Text == nil {
				return string(raw), nil
			}
			texts = append(texts, *p.Text)
		}
		return strings.Join(texts, "\n"), nil
	}
	return "", inputError("invalid_request", "消息 content 需要字符串、内容数组或 null")
}
