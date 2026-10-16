package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestChatProbeReadOnlyAndRedacted(t *testing.T) {
	s := testServer(t)
	s.accounts = []Account{{ID: "fixture", Cookie: "session=secret"}}
	seen := make(map[string]bool)
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Host != "hark.com" || r.Header.Get("Cookie") != "session=secret" {
			t.Fatal("wrong probe request")
		}
		seen[r.URL.Path] = true
		body := `{"session":{"id":"private-session"},"user":{"id":"private-user"}}`
		if r.URL.Path == "/api/conversations" {
			body = `{"conversations":[{"id":"private-project","title":"private-content"}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	w := call(s, "POST", "/admin/accounts/fixture/chat-probe", `{}`, "")
	assertStatus(t, w, 200)
	if len(seen) != 2 || !strings.Contains(w.Body.String(), `"session_valid":true`) || !strings.Contains(w.Body.String(), `"conversations":1`) || !strings.Contains(w.Body.String(), `"project_protocol":"verified"`) || !strings.Contains(w.Body.String(), `"chat_ready":false`) {
		t.Fatal(w.Body.String())
	}
	for _, secret := range []string{"private-session", "private-user", "private-project", "private-content", "session=secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private data exposed", secret)
		}
	}
	assertStatus(t, call(s, "POST", "/admin/accounts/missing/chat-probe", `{}`, ""), 404)
}
