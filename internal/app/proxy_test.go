package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSetUpstreamProxy(t *testing.T) {
	s := testServer(t)
	if err := s.SetUpstreamProxy("http://127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	transport, ok := s.client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatal("proxy transport not installed")
	}
	req, _ := http.NewRequest("GET", "https://hark.com/", nil)
	proxy, err := transport.Proxy(req)
	if err != nil || proxy.String() != "http://127.0.0.1:7897" {
		t.Fatalf("proxy=%v err=%v", proxy, err)
	}
	for _, raw := range []string{"", "https://127.0.0.1:7897", "http://127.0.0.1:7897/path", "http://user:pass@127.0.0.1:7897"} {
		if err := s.SetUpstreamProxy(raw); raw != "" && err == nil {
			t.Fatalf("accepted invalid proxy %q", raw)
		}
	}
	proxy, err = transport.Proxy(req)
	if err != nil || proxy.String() != "http://127.0.0.1:7897" {
		t.Fatal("original transport changed")
	}
	if err := s.SetUpstreamProxy(""); err != nil {
		t.Fatal(err)
	}
	if s.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("explicit direct mode not applied")
	}
}

func TestProxyCONNECTFailureDoesNotFallback(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "CONNECT" || r.Host != "hark.com:443" {
			t.Errorf("wrong tunnel %s %s", r.Method, r.Host)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	s := testServer(t)
	if err := s.SetUpstreamProxy(proxy.URL); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	err := s.harkJSON(context.Background(), "GET", "/api/auth/get-session", "session=fixture", nil, &output)
	if err == nil || calls.Load() != 1 {
		t.Fatal("proxy failure retried or bypassed", err, calls.Load())
	}
}
