package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sessionFixture struct {
	mu                       sync.Mutex
	created, archived, sends int
	archiveFailure           bool
	ids, requests            map[string]string
	messages                 []string
	block, entered           chan struct{}
}

func fixtureSessions(s *Server) *sessionFixture {
	f := &sessionFixture{ids: map[string]string{}, requests: map[string]string{}}
	s.accounts = []Account{{ID: "fixture-account", Name: "fixture", Status: "valid", Cookie: "session=fixture"}}
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, status := "{}", 200
		switch {
		case r.URL.Path == "/api/conversations":
			f.created++
			body = fmt.Sprintf(`{"id":"c%d"}`, f.created)
		case r.URL.Path == "/api/messages/send":
			f.sends++
			var input map[string]string
			json.NewDecoder(r.Body).Decode(&input)
			cid := input["conversationId"]
			f.ids[cid], f.requests[cid] = input["responseMessageId"], input["idempotencyKey"]
			f.messages = append(f.messages, input["message"])
			if f.block != nil {
				block := f.block
				f.block = nil
				close(f.entered)
				f.mu.Unlock()
				<-block
				f.mu.Lock()
			}
			body = fmt.Sprintf(`{"success":true,"conversationId":%q,"messageId":%q,"redirected":false}`, cid, input["idempotencyKey"])
		case r.URL.Path == "/api/messages/history":
			cid := r.URL.Query().Get("conversationId")
			body = fmt.Sprintf(`{"messages":[{"id":%q,"role":"assistant","triggeredByMessageId":%q,"content":"fixture reply","jobStatus":"completed","isStreaming":false}]}`, f.ids[cid], f.requests[cid])
		case strings.HasSuffix(r.URL.Path, "/archive"):
			f.archived++
			if f.archiveFailure {
				status = 503
			}
		default:
			status = 404
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return f
}

func sessionKey(t *testing.T, s *Server) string {
	t.Helper()
	w := call(s, "POST", "/admin/keys", `{"name":"fixture-key"}`, "")
	assertStatus(t, w, 201)
	var result struct {
		Secret string `json:"secret"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	return result.Secret
}

func sessionCall(s *Server, key, id string, end bool) *httptest.ResponseRecorder {
	return call(s, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"hark","messages":[{"role":"user","content":"hello"}],"session_id":%q,"session_end":%t}`, id, end), key)
}

func TestSessionReuseIsolationAndPersistence(t *testing.T) {
	s := testServer(t)
	f := fixtureSessions(s)
	k1, k2 := sessionKey(t, s), sessionKey(t, s)
	for i, tc := range []struct {
		key, id string
		reused  bool
	}{
		{k1, "one", false}, {k1, "one", true}, {k1, "two", false}, {k2, "one", false}, {k1, "", false}, {k1, "", false},
	} {
		w := sessionCall(s, tc.key, tc.id, false)
		assertStatus(t, w, 200)
		if w.Header().Get("X-Hark-Project-Reused") != fmt.Sprint(tc.reused) {
			t.Fatal(i, w.Header())
		}
	}
	if f.created != 5 || f.sends != 6 || f.archived != 0 {
		t.Fatal(f)
	}
	b, err := os.ReadFile(filepath.Join(s.data, "project-sessions.json"))
	if err != nil || strings.Contains(string(b), k1) || strings.Contains(string(b), "session=fixture") || strings.Contains(string(b), `"one"`) {
		t.Fatal("store leaked inputs or missing", err)
	}
	reloaded, err := New(s.data)
	if err != nil {
		t.Fatal(err)
	}
	f2 := fixtureSessions(reloaded)
	w := sessionCall(reloaded, k1, "one", false)
	assertStatus(t, w, 200)
	if f2.created != 0 || w.Header().Get("X-Hark-Project-Reused") != "true" {
		t.Fatal("restart lost mapping")
	}
	// A session stays bound to its account instead of silently losing context.
	reloaded.accounts = []Account{{ID: "different", Status: "valid", Cookie: "session=fixture"}}
	assertStatus(t, sessionCall(reloaded, k1, "one", false), 409)
}

func TestSessionEndArchiveFailureAndIdle(t *testing.T) {
	s := testServer(t)
	f := fixtureSessions(s)
	key := sessionKey(t, s)
	f.archiveFailure = true
	w := sessionCall(s, key, "one", true)
	assertStatus(t, w, 200)
	if w.Header().Get("X-Hark-Project-Archived") != "false" || len(s.projects) != 1 {
		t.Fatal("failed archive lost mapping")
	}
	assertStatus(t, sessionCall(s, key, "one", false), 409)
	f.archiveFailure = false
	s.cleanupProjects(time.Now(), 30*time.Minute)
	if len(s.projects) != 0 || f.archived != 2 {
		t.Fatal("ending archive not retried")
	}
	assertStatus(t, sessionCall(s, key, "one", false), 200)
	if f.created != 2 {
		t.Fatal("archived project reused")
	}
	s.cleanupProjects(time.Now().Add(29*time.Minute), 30*time.Minute)
	if f.archived != 2 {
		t.Fatal("early archive")
	}
	for _, p := range s.projects {
		p.Busy = true
	}
	s.cleanupProjects(time.Now().Add(time.Hour), 30*time.Minute)
	if f.archived != 2 {
		t.Fatal("busy project archived")
	}
	for _, p := range s.projects {
		p.Busy = false
	}
	f.archiveFailure = true
	s.cleanupProjects(time.Now().Add(time.Hour), 30*time.Minute)
	if len(s.projects) != 1 {
		t.Fatal("idle failure lost context")
	}
	f.archiveFailure = false
	s.cleanupProjects(time.Now().Add(time.Hour), 30*time.Minute)
	if len(s.projects) != 0 {
		t.Fatal("idle mapping not removed")
	}
	w = sessionCall(s, key, "one", true)
	assertStatus(t, w, 200)
	if w.Header().Get("X-Hark-Project-Archived") != "true" || len(s.projects) != 0 {
		t.Fatal("end not archived")
	}
}

func TestSessionConcurrentRequests(t *testing.T) {
	s := testServer(t)
	f := fixtureSessions(s)
	key := sessionKey(t, s)
	block := make(chan struct{})
	f.block, f.entered = block, make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- sessionCall(s, key, "busy", false) }()
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send did not enter")
	}
	w := sessionCall(s, key, "busy", false)
	assertStatus(t, w, 409)
	if !strings.Contains(w.Body.String(), "session_busy") {
		t.Fatal(w.Body.String())
	}
	assertStatus(t, sessionCall(s, key, "independent", false), 200)
	close(block)
	assertStatus(t, <-done, 200)
	if f.created != 2 || f.sends != 2 {
		t.Fatal("duplicate send")
	}
}

func TestSessionHeadersAndValidation(t *testing.T) {
	s := testServer(t)
	f := fixtureSessions(s)
	key := sessionKey(t, s)
	for _, end := range []bool{false, true} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8789/v1/chat/completions", strings.NewReader(`{"model":"hark","messages":[{"role":"user","content":"hi"}],"session_id":"body"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("X-Hark-Session-Id", "header")
		r.Header.Set("X-Hark-Session-End", fmt.Sprint(end))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		assertStatus(t, w, 200)
		if w.Header().Get("X-Hark-Project-Reused") != fmt.Sprint(end) {
			t.Fatal("header precedence")
		}
	}
	if f.created != 1 || f.archived != 1 {
		t.Fatal(f)
	}
	for _, fields := range []string{`"session_id":42`, `"session_id":null`, `"session_end":"true"`, `"session_end":true`, `"session_id":" bad"`, `"session_id":"a\nb"`} {
		w := call(s, "POST", "/v1/chat/completions", `{"model":"hark","messages":[{"role":"user","content":"hi"}],`+fields+`}`, key)
		assertStatus(t, w, 400)
	}
	// Admin test requests never share a Project with external Keys.
	r := httptest.NewRequest("POST", "http://127.0.0.1:8789/admin/playground/chat", nil)
	api := httptest.NewRequest("POST", "http://127.0.0.1:8789/v1/chat/completions", nil)
	api.Header.Set("Authorization", "Bearer "+key)
	if projectMapping(r, "one") == projectMapping(api, "one") {
		t.Fatal("admin/key namespace collision")
	}
}

func TestSessionTranscriptAndProxyArchive(t *testing.T) {
	v, _, err := normalizeChat([]byte(`{"model":"hark","messages":[{"role":"system","content":"rules"},{"role":"user","content":"old"},{"role":"assistant","content":"previous"},{"role":"user","content":"latest"}],"session_id":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	text := transcript(v)
	if !strings.Contains(text, "rules") || !strings.Contains(text, "latest") || !strings.Contains(text, "previous") || !strings.Contains(text, "old") {
		t.Fatal(text)
	}
	s := testServer(t)
	if err := s.SetUpstreamProxy("http://127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	transport := s.client.Transport.(*http.Transport)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://hark.com/api/conversations/fixture/archive", nil)
	proxy, err := transport.Proxy(req)
	if err != nil || proxy.String() != "http://127.0.0.1:7897" {
		t.Fatal("archive bypassed configured proxy")
	}
}
