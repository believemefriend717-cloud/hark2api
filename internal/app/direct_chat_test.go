package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixtureDirect(t *testing.T, s *Server) *int {
	t.Helper()
	var mu sync.Mutex
	count := 0
	ids := map[string]string{}
	requests := map[string]string{}
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host != "hark.com" || r.Header.Get("Cookie") != "session=fixture" {
			t.Fatal("wrong host or credentials")
		}
		body := ""
		switch r.URL.Path {
		case "/api/conversations":
			if r.Method != "POST" {
				t.Fatal("must create independent conversation")
			}
			count++
			var input map[string]any
			if json.NewDecoder(r.Body).Decode(&input) != nil || input["title"] != "Hark2API Project" || input["autoTitle"] != true {
				t.Fatal("missing Project creation fields")
			}
			body = fmt.Sprintf(`{"conversationId":"c%d"}`, count)
		case "/api/messages/send":
			var data map[string]any
			if json.NewDecoder(r.Body).Decode(&data) != nil || r.Method != "POST" {
				t.Fatal("invalid send")
			}
			cid := r.URL.Query().Get("cid")
			if data["conversationId"] != cid || data["responseMessageId"] == "" || data["idempotencyKey"] == "" || data["message"] == "" {
				t.Fatal(data)
			}
			if _, ok := ids[cid]; ok {
				t.Fatal("send replayed")
			}
			ids[cid] = data["responseMessageId"].(string)
			requests[cid] = data["idempotencyKey"].(string)
			body = fmt.Sprintf(`{"success":true,"conversationId":%q,"messageId":%q,"redirected":false}`, cid, data["idempotencyKey"])
		case "/api/messages/history":
			id := ids[r.URL.Query().Get("conversationId")]
			if id == "" {
				t.Fatal("unbound history")
			}
			requestID := requests[r.URL.Query().Get("conversationId")]
			body = fmt.Sprintf(`{"messages":[{"id":"other","role":"assistant","content":"wrong","jobStatus":"completed"},{"id":%q,"role":"assistant","triggeredByMessageId":%q,"content":"","jobStatus":"completed","isStreaming":false},{"id":"bubble","role":"assistant","triggeredByMessageId":%q,"content":"fixture answer","source":"send_bubble","presentation":{"kind":"text"},"isStreaming":false}]}`, id, requestID, requestID)
		default:
			t.Fatal("unexpected upstream path", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return &count
}

func TestDirectJSONSSEAndIsolation(t *testing.T) {
	s := testServer(t)
	s.accounts = []Account{{ID: "a", Status: "valid", Cookie: "session=fixture"}}
	count := fixtureDirect(t, s)
	for _, stream := range []bool{false, true} {
		w := call(s, "POST", "/admin/keys", `{"name":"client"}`, "")
		var key struct {
			Secret string `json:"secret"`
		}
		json.Unmarshal(w.Body.Bytes(), &key)
		body := fmt.Sprintf(`{"model":"hark","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
		w = call(s, "POST", "/v1/chat/completions", body, key.Secret)
		assertStatus(t, w, 200)
		if w.Header().Get("X-Hark-Session-Mode") != "project" {
			t.Fatal("wrong session mode")
		}
		if strings.Contains(w.Body.String(), "wrong") || !strings.Contains(w.Body.String(), "fixture answer") {
			t.Fatal(w.Body.String())
		}
		if stream {
			if w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), "data: [DONE]") || w.Header().Get("X-Hark-Stream-Mode") != "buffered" {
				t.Fatal(w.Body.String())
			}
		} else {
			var v map[string]any
			if json.Unmarshal(w.Body.Bytes(), &v) != nil || v["object"] != "chat.completion" {
				t.Fatal(w.Body.String())
			}
		}
	}
	if *count != 2 {
		t.Fatal("requests shared conversation")
	}
}

func TestDirectFailureNeverRepeatsSend(t *testing.T) {
	for _, code := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := testServer(t)
			calls := 0
			s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("private secret response")), Header: make(http.Header)}, nil
			})}
			_, err := s.runDirect(context.Background(), "session=fixture", "UTC", chatRequest{}, time.Millisecond)
			if err == nil || calls != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestReplyCorrelationAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		m      harkMessage
		want   string
		failed bool
	}{
		{harkMessage{ID: "other", Role: "assistant", Content: "wrong", Status: "completed"}, "", false},
		{harkMessage{ID: "expected", Role: "assistant", Content: "partial", Status: "running"}, "", false},
		{harkMessage{ID: "expected", Role: "assistant", Content: "unknown"}, "", true},
		{harkMessage{ID: "expected", Role: "assistant", Content: "answer", Status: "completed"}, "answer", false},
		{harkMessage{ID: "expected", Role: "assistant", Status: "failed"}, "", true},
	} {
		text, err := completedReply([]harkMessage{tc.m}, "expected", "")
		if text != tc.want || (err != nil) != tc.failed {
			t.Fatal(tc, text, err)
		}
	}
}

func TestTranscriptPreservesClientHistory(t *testing.T) {
	var v chatRequest
	json.Unmarshal([]byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"next"}]}`), &v)
	for _, s := range []string{"rules", "first", "reply", "next"} {
		if !strings.Contains(transcript(v), s) {
			t.Fatal("history lost")
		}
	}
}

func TestDirectStopsOnCancellationAndUnknownProtocol(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		s := testServer(t)
		sends := 0
		expected := ""
		requestID := ""
		s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body string
			switch r.URL.Path {
			case "/api/conversations":
				body = `{"id":"isolated"}`
			case "/api/messages/send":
				sends++
				var input map[string]string
				json.NewDecoder(r.Body).Decode(&input)
				expected = input["responseMessageId"]
				requestID = input["idempotencyKey"]
				body = fmt.Sprintf(`{"success":true,"conversationId":"isolated","messageId":%q,"redirected":false}`, requestID)
			case "/api/messages/history":
				status := `,"status":"running"`
				if unknown {
					status = ""
				}
				body = fmt.Sprintf(`{"messages":[{"id":%q,"role":"assistant","triggeredByMessageId":%q,"content":"partial"%s}]}`, expected, requestID, status)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		text, err := s.runDirect(ctx, "session=fixture", "UTC", chatRequest{}, time.Second)
		cancel()
		if err == nil || text != "" || sends != 1 {
			t.Fatal("partial reply returned or send repeated", text, err, sends)
		}
		if unknown && !strings.Contains(err.Error(), "完成字段") {
			t.Fatal(err)
		}
	}
}

func TestProjectBubblesRequireBoundCompletedJobs(t *testing.T) {
	streaming, done := true, false
	job := harkMessage{ID: "expected", Role: "assistant", TriggeredBy: "request", JobStatus: "completed", Streaming: &done}
	bubble := harkMessage{ID: "bubble", Role: "assistant", TriggeredBy: "request", Source: "send_bubble", Content: "answer", Streaming: &done}
	bubble.Presentation.Kind = "text"
	for _, tc := range []struct {
		name     string
		messages []harkMessage
		want     string
	}{
		{"completed", []harkMessage{job, bubble}, "answer"},
		{"bubble_only", []harkMessage{bubble}, ""},
		{"other_request", []harkMessage{job, {ID: "other", Role: "assistant", TriggeredBy: "other", Content: "wrong", Streaming: &done}}, ""},
		{"followup_running", []harkMessage{job, bubble, {ID: "followup", Role: "assistant", TriggeredBy: "request", JobStatus: "running", Streaming: &streaming}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := completedReply(tc.messages, "expected", "request")
			if got != tc.want || err != nil {
				t.Fatal(got, err)
			}
		})
	}
}

func TestProjectRedirectRejectedWithoutRetry(t *testing.T) {
	for _, redirected := range []bool{false, true} {
		s := testServer(t)
		calls := 0
		s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			body := `{"conversationId":"new-project"}`
			if calls == 2 {
				var input map[string]string
				json.NewDecoder(r.Body).Decode(&input)
				cid := "main"
				if redirected {
					cid = "new-project"
				}
				body = fmt.Sprintf(`{"success":true,"conversationId":%q,"messageId":%q,"redirected":%t}`, cid, input["idempotencyKey"], redirected)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		_, err := s.runDirect(context.Background(), "session=fixture", "UTC", chatRequest{}, time.Millisecond)
		if e, ok := err.(*upstreamError); !ok || e.code != "upstream_project_mismatch" || calls != 2 {
			t.Fatal(err, calls)
		}
	}
}
