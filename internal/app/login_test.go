package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type bridgeTransport func(*http.Request) (*http.Response, error)

func (f bridgeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func validSessionClient(s *Server) {
	s.client = &http.Client{Transport: bridgeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != harkSessionURL || r.Header.Get("Cookie") != "__Secure-hark.session_token=fixture-session" {
			return nil, fmt.Errorf("unexpected session request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"session":{"id":"s"},"user":{"id":"u","name":"Fixture user"}}`)), Header: make(http.Header)}, nil
	})}
}
func bridgeAttempt(t *testing.T, s *Server) (string, string) {
	t.Helper()
	w := call(s, "POST", "/admin/login/start", `{"name":"Imported"}`, "")
	assertStatus(t, w, 201)
	var d struct {
		ID    string `json:"id"`
		Token string `json:"bridge_token"`
		URL   string `json:"auth_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.URL != harkLoginURL || len(d.Token) < 48 {
		t.Fatal("invalid browser-owned login initialization")
	}
	return d.ID, d.Token
}
func bridgeBody(id, token string) string {
	b, _ := json.Marshal(map[string]string{"id": id, "token": token, "cookie": "__Secure-hark.session_token=fixture-session"})
	return string(b)
}
func TestBridgeImport(t *testing.T) {
	s := testServer(t)
	validSessionClient(s)
	id, token := bridgeAttempt(t, s)
	assertStatus(t, call(s, "POST", "/admin/login/complete", fmt.Sprintf(`{"id":%q}`, id), ""), 409)
	assertStatus(t, call(s, "POST", "/bridge/login", bridgeBody(id, "wrong"), ""), 403)
	w := call(s, "POST", "/bridge/login", bridgeBody(id, token), "")
	assertStatus(t, w, 201)
	if strings.Contains(w.Body.String(), "fixture-session") {
		t.Fatal("cookie in bridge response")
	}
	status := call(s, "GET", "/admin/login/status?id="+id, "", "")
	assertStatus(t, status, 200)
	if !strings.Contains(status.Body.String(), "complete") {
		t.Fatal("not complete")
	}
	assertStatus(t, call(s, "POST", "/bridge/login", bridgeBody(id, token), ""), 410)
	w = call(s, "POST", "/admin/login/complete", fmt.Sprintf(`{"id":%q}`, id), "")
	assertStatus(t, w, 201)
	if !strings.Contains(w.Body.String(), "fixture-session") || len(s.accounts) != 1 {
		t.Fatal("session not delivered")
	}
	assertStatus(t, call(s, "POST", "/admin/login/complete", fmt.Sprintf(`{"id":%q}`, id), ""), 410)
	if strings.Contains(call(s, "GET", "/admin/state", "", "").Body.String(), "fixture-session") {
		t.Fatal("cookie leaked in state/log")
	}
}
func TestBridgeExpiredCancelledInvalidAndConcurrent(t *testing.T) {
	s := testServer(t)
	validSessionClient(s)
	id, token := bridgeAttempt(t, s)
	s.attempt(id).expires = time.Now().Add(-time.Second)
	assertStatus(t, call(s, "POST", "/bridge/login", bridgeBody(id, token), ""), 410)
	s.removeLogin(id)
	id, token = bridgeAttempt(t, s)
	s.removeLogin(id)
	assertStatus(t, call(s, "POST", "/bridge/login", bridgeBody(id, token), ""), 410)
	id, token = bridgeAttempt(t, s)
	original := s.client
	s.client = &http.Client{Transport: bridgeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("null")), Header: make(http.Header)}, nil
	})}
	assertStatus(t, call(s, "POST", "/bridge/login", bridgeBody(id, token), ""), 409)
	if len(s.accounts) != 0 {
		t.Fatal("invalid session imported")
	}
	s.client = original
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- call(s, "POST", "/bridge/login", bridgeBody(id, token), "").Code }()
	}
	wg.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		if code == 201 {
			successes++
		} else if code != 410 {
			t.Fatalf("unexpected code %d", code)
		}
	}
	if successes != 1 || len(s.accounts) != 1 {
		t.Fatal("concurrent replay imported twice")
	}
}

// Used only by the real-extension E2E runner on a random loopback port. The
// transport validates the exact synthetic HttpOnly Cookie; no Hark network.
func TestBridgeBrowserFixture(t *testing.T) {
	addr := os.Getenv("HARK_BRIDGE_FIXTURE_ADDR")
	if addr == "" {
		t.Skip("browser fixture only")
	}
	s := testServer(t)
	validSessionClient(s)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: s.Handler()}
	defer server.Close()
	go server.Serve(listener)
	fmt.Println("FIXTURE_READY")
	<-time.After(100 * time.Second)
}
