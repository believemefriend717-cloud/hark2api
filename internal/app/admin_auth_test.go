package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func authServer(t *testing.T) *Server {
	t.Helper()
	hash := sha256.Sum256([]byte("fixture-admin-key"))
	t.Setenv("HARK_ADMIN_KEY_SHA256", hex.EncodeToString(hash[:]))
	t.Setenv("HARK_PUBLIC_ORIGIN", "https://gateway.example.invalid:18789")
	return testServer(t)
}
func remoteCall(s *Server, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	method := "GET"
	if body != "" {
		method = "POST"
	}
	r := httptest.NewRequest(method, "https://gateway.example.invalid:18789"+path, strings.NewReader(body))
	r.Header.Set("X-Hark-Admin", "1")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestRemoteAdminAuth(t *testing.T) {
	s := authServer(t)
	assertStatus(t, remoteCall(s, "/admin/state", "", nil, ""), 401)
	assertStatus(t, remoteCall(s, "/admin/auth/login", `{"key":"wrong"}`, nil, ""), 401)
	assertStatus(t, remoteCall(s, "/admin/auth/login", `{"key":"fixture-admin-key"}`, nil, "https://other.example"), 403)
	w := remoteCall(s, "/admin/auth/login", `{"key":"fixture-admin-key"}`, nil, "https://gateway.example.invalid:18789")
	assertStatus(t, w, 200)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure admin cookie")
	}
	state := remoteCall(s, "/admin/state", "", cookies[0], "")
	assertStatus(t, state, 200)
	if !strings.Contains(state.Body.String(), "https://gateway.example.invalid:18789/v1") {
		t.Fatal("wrong public endpoint")
	}
	assertStatus(t, remoteCall(s, "/admin/login/start", `{}`, cookies[0], ""), 201)
	assertStatus(t, remoteCall(s, "/admin/auth/logout", `{}`, cookies[0], ""), 200)
	assertStatus(t, remoteCall(s, "/admin/state", "", cookies[0], ""), 401)
	assertStatus(t, remoteCall(s, "/v1/models", "", nil, ""), 401)
}
func TestAdminLoginRateLimitAndConfig(t *testing.T) {
	s := authServer(t)
	for i := 0; i < 5; i++ {
		assertStatus(t, remoteCall(s, "/admin/auth/login", `{"key":"wrong"}`, nil, ""), 401)
	}
	assertStatus(t, remoteCall(s, "/admin/auth/login", `{"key":"wrong"}`, nil, ""), 429)
	t.Setenv("HARK_ADMIN_KEY_SHA256", "")
	if _, e := New(t.TempDir()); e == nil {
		t.Fatal("remote access without key accepted")
	}
}

func TestRemoteHTTPAdminAuth(t *testing.T) {
	s := authServer(t)
	t.Setenv("HARK_PUBLIC_ORIGIN", "http://gateway.example.invalid:18789")
	s = testServer(t)
	call := func(path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		method := http.MethodGet
		if body != "" {
			method = http.MethodPost
		}
		r := httptest.NewRequest(method, s.publicOrigin+path, strings.NewReader(body))
		r.Header.Set("X-Hark-Admin", "1")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	assertStatus(t, call("/admin/state", "", "", nil), 401)
	assertStatus(t, call("/admin/auth/login", `{"key":"fixture-admin-key"}`, "https://gateway.example.invalid:18789", nil), 403)
	assertStatus(t, call("/admin/auth/login", `{"key":"wrong"}`, s.publicOrigin, nil), 401)
	w := call("/admin/auth/login", `{"key":"fixture-admin-key"}`, s.publicOrigin, nil)
	assertStatus(t, w, 200)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "hark_admin_http" || cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("incorrect HTTP cookie flags")
	}
	state := call("/admin/state", "", "", cookies[0])
	assertStatus(t, state, 200)
	if !strings.Contains(state.Body.String(), s.publicOrigin+"/v1") {
		t.Fatal("wrong HTTP endpoint")
	}
	assertStatus(t, call("/admin/auth/logout", `{}`, s.publicOrigin, cookies[0]), 200)
	assertStatus(t, call("/admin/state", "", "", cookies[0]), 401)
	assertStatus(t, call("/v1/models", "", "", nil), 401)
}

func TestPublicOriginValidation(t *testing.T) {
	hash := sha256.Sum256([]byte("fixture-admin-key"))
	t.Setenv("HARK_ADMIN_KEY_SHA256", hex.EncodeToString(hash[:]))
	for _, origin := range []string{"ftp://gateway.example.invalid:18789", "http://user@gateway.example.invalid:18789", "http://gateway.example.invalid:18789/path", "http://gateway.example.invalid:18789?q=1", "http://gateway.example.invalid:18789#fragment"} {
		t.Setenv("HARK_PUBLIC_ORIGIN", origin)
		if _, err := New(t.TempDir()); err == nil {
			t.Fatalf("invalid origin accepted: %s", origin)
		}
	}
}
