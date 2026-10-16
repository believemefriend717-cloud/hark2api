package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"
)

type probeResult struct {
	Path         string         `json:"path"`
	Status       int            `json:"http_status,omitempty"`
	ContentType  string         `json:"content_type,omitempty"`
	Keys         []string       `json:"json_keys,omitempty"`
	ArrayLengths map[string]int `json:"array_lengths,omitempty"`
	SessionValid bool           `json:"session_valid,omitempty"`
	Outcome      string         `json:"outcome"`
}

// Read-only probes reveal response shape, never account data or credential values.
func (s *Server) chatProbe(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var cookie string
	var chatVerified bool
	for _, a := range s.accounts {
		if a.ID == r.PathValue("id") {
			cookie = a.Cookie
			chatVerified = a.ChatVerified && a.Status == "valid"
			break
		}
	}
	s.mu.Unlock()
	if cookie == "" {
		fail(w, 404, "not_found", "账号不存在，请重新导入 Cookie")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results := make([]probeResult, 0, 2)
	for _, path := range []string{"/api/auth/get-session", "/api/conversations"} {
		result := probeResult{Path: path}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://hark.com"+path, nil)
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Accept", "application/json")
		resp, err := s.client.Do(req)
		if err != nil {
			result.Outcome = "connection_failed"
			results = append(results, result)
			continue
		}
		result.Status, result.ContentType = resp.StatusCode, resp.Header.Get("Content-Type")
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 65537))
		resp.Body.Close()
		switch {
		case resp.StatusCode == 403:
			result.Outcome = "access_blocked"
		case resp.StatusCode == 401:
			result.Outcome = "unauthorized"
		case resp.StatusCode == 404:
			result.Outcome = "route_not_found"
		case resp.StatusCode == 429:
			result.Outcome = "rate_limited"
		case resp.StatusCode != 200:
			result.Outcome = "http_error"
		case readErr != nil || len(body) > 65536:
			result.Outcome = "response_read_failed"
		default:
			var object map[string]json.RawMessage
			if json.Unmarshal(body, &object) != nil {
				result.Outcome = "non_object_response"
				break
			}
			if object == nil {
				result.Outcome = "empty_session"
				break
			}
			result.Outcome = "json_object"
			for key, raw := range object {
				result.Keys = append(result.Keys, key)
				var items []json.RawMessage
				if json.Unmarshal(raw, &items) == nil && items != nil {
					if result.ArrayLengths == nil {
						result.ArrayLengths = make(map[string]int)
					}
					result.ArrayLengths[key] = len(items)
				}
			}
			sort.Strings(result.Keys)
			if path == "/api/auth/get-session" {
				var session, user struct {
					ID string `json:"id"`
				}
				result.SessionValid = json.Unmarshal(object["session"], &session) == nil && json.Unmarshal(object["user"], &user) == nil && session.ID != "" && user.ID != ""
			}
		}
		results = append(results, result)
	}
	if len(results) == 0 || !results[0].SessionValid {
		chatVerified = false
	}
	write(w, 200, map[string]any{"results": results, "chat_ready": chatVerified, "upstream_sent": false, "project_protocol": "verified", "session_mode": "project", "chat_verification": "last_successful_api_call", "project_create_fields": []string{"title", "timezone", "autoTitle"}})
}
