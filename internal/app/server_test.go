package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func call(s *Server, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8789"+path, strings.NewReader(body))
	r.Header.Set("X-Hark-Admin", "1")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func assertStatus(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status=%d want=%d body=%s", w.Code, code, w.Body.String())
	}
}

func TestKeysPersistHashAndRevoke(t *testing.T) {
	s := testServer(t)
	assertStatus(t, call(s, "GET", "/v1/models", "", ""), 401)
	w := call(s, "POST", "/admin/keys", `{"name":"test client"}`, "")
	assertStatus(t, w, 201)
	var created struct {
		Key    Key    `json:"key"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Secret == "" || created.Key.Hash != "" {
		t.Fatal("key creation leaked hash or omitted secret")
	}
	config, err := os.ReadFile(filepath.Join(s.data, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), created.Secret) {
		t.Fatal("plaintext key persisted")
	}
	reloaded, err := New(s.data)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, call(reloaded, "GET", "/v1/models", "", created.Secret), 200)
	state := call(reloaded, "GET", "/admin/state", "", "").Body.String()
	if strings.Contains(state, created.Secret) || strings.Contains(state, "\"hash\"") {
		t.Fatal("secret or hash exposed by state")
	}
	assertStatus(t, call(reloaded, "POST", "/v1/chat/completions", `{"model":"hark","messages":[{"role":"user","content":"hi"}]}`, created.Secret), 409)
	assertStatus(t, call(reloaded, "DELETE", "/admin/keys/"+created.Key.ID, "", ""), 200)
	assertStatus(t, call(reloaded, "GET", "/v1/models", "", created.Secret), 401)
	after, err := New(s.data)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, call(after, "GET", "/v1/models", "", created.Secret), 401)
}

func TestAccountsPersistEncryptedAndNeverExposeCookie(t *testing.T) {
	s := testServer(t)
	w := call(s, "POST", "/admin/accounts", `{"name":"my Hark","cookie":"session=secret-sample"}`, "")
	assertStatus(t, w, 201)
	if strings.Contains(w.Body.String(), "secret-sample") {
		t.Fatal("cookie exposed on creation")
	}
	var a Account
	json.Unmarshal(w.Body.Bytes(), &a)
	if strings.Contains(call(s, "GET", "/admin/state", "", "").Body.String(), "secret-sample") {
		t.Fatal("cookie exposed in state")
	}
	assertStatus(t, call(s, "PUT", "/admin/settings", `{"name":"Saved","timezone":"UTC","timeout":90}`, ""), 200)
	b, _ := os.ReadFile(filepath.Join(s.data, "config.json"))
	if strings.Contains(string(b), "secret-sample") {
		t.Fatal("cookie persisted")
	}
	reloaded, err := New(s.data)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.accounts) != 1 || reloaded.accounts[0].Cookie != "session=secret-sample" {
		t.Fatal("accounts not restored across restart")
	}
	ciphertext, _ := os.ReadFile(filepath.Join(s.data, "accounts.enc"))
	if strings.Contains(string(ciphertext), "secret-sample") {
		t.Fatal("plaintext Cookie persisted")
	}
	assertStatus(t, call(s, "DELETE", "/admin/accounts/"+a.ID, "", ""), 200)
	after, err := New(s.data)
	if err != nil || len(after.accounts) != 0 {
		t.Fatal("deleted account restored", err)
	}
	assertStatus(t, call(s, "DELETE", "/admin/accounts/"+a.ID, "", ""), 404)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionChecksClassifyResponses(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		body, want string
	}{
		{"empty", 200, "null", "expired"},
		{"valid", 200, `{"session":{"id":"s"},"user":{"id":"u"}}`, "valid"},
		{"blocked", 403, "<html>Cloudflare</html>", "blocked"},
		{"auth", 401, `{"error":"unauthorized"}`, "expired"},
		{"limited", 429, `{}`, "limited"},
		{"unknown", 200, `{"data":"unexpected"}`, "error"},
		{"empty_objects", 200, `{"session":{},"user":{}}`, "error"},
		{"bad_json", 200, "<html>Login</html>", "error"},
		{"too_large", 200, strings.Repeat("x", 65537), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			s.accounts = []Account{{ID: "one", Name: "sample", Cookie: "session=test", Status: "unchecked"}}
			s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://hark.com/api/auth/get-session" || r.Header.Get("Cookie") != "session=test" {
					t.Fatal("wrong request target or cookie")
				}
				return &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			w := call(s, "POST", "/admin/accounts/one/check", `{}`, "")
			assertStatus(t, w, 200)
			var a Account
			json.Unmarshal(w.Body.Bytes(), &a)
			if a.Status != tc.want || a.Checked == "" {
				t.Fatalf("status=%s want=%s", a.Status, tc.want)
			}
			if strings.Contains(w.Body.String(), "session=test") {
				t.Fatal("cookie leaked")
			}
		})
	}
}

func TestAdminRejectsCrossOriginAndRebinding(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct{ name, host, origin, site, marker string }{
		{"missing_marker", "127.0.0.1:8789", "", "", ""},
		{"evil_origin", "127.0.0.1:8789", "https://example.com", "", "1"},
		{"cross_site", "127.0.0.1:8789", "", "cross-site", "1"},
		{"rebind", "attacker.example:8789", "", "", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+tc.host+"/admin/keys", strings.NewReader(`{"name":"x"}`))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("X-Hark-Admin", tc.marker)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			assertStatus(t, w, 403)
		})
	}
	if len(s.keys) != 0 {
		t.Fatal("blocked request changed state")
	}
}

func TestSettingsRejectInvalidAndPersistValid(t *testing.T) {
	s := testServer(t)
	for _, body := range []string{
		`{"name":"","timezone":"UTC","timeout":90}`,
		`{"name":"test","timezone":"Invalid/Zone","timeout":90}`,
		`{"name":"test","timezone":"UTC","timeout":1}`,
		`{"name":"test","timezone":"UTC","timeout":601}`,
		`{"name":"test","timezone":"UTC","timeout":90,"extra":true}`,
	} {
		assertStatus(t, call(s, "PUT", "/admin/settings", body, ""), 400)
	}
	if s.settings.Name != "Hark Gateway" {
		t.Fatal("invalid input modified settings")
	}
	assertStatus(t, call(s, "PUT", "/admin/settings", `{"name":"My Hark","timezone":"Asia/Tokyo","timeout":90}`, ""), 200)
	reloaded, err := New(s.data)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.settings.Name != "My Hark" || reloaded.settings.Timeout != 90 {
		t.Fatal("valid settings did not persist")
	}
}

func TestRequestValidationHasNoUpstreamEffects(t *testing.T) {
	s := testServer(t)
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("validator sent upstream request")
		return nil, nil
	})}
	assertStatus(t, call(s, "POST", "/admin/validate", `{"model":"hark","messages":[{"role":"user","content":"hello"}],"stream":false}`, ""), 200)
	for _, body := range []string{
		`{"model":"other","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"hark","messages":[]}`,
		`{"model":"hark","messages":[{"role":"user","content":""}]}`,
		`{"model":"hark","messages":[{"content":"hello"}]}`,
		`{"model":"hark","messages":[{"role":"user","content":123}]}`,
		`{} {}`,
	} {
		assertStatus(t, call(s, "POST", "/admin/validate", body, ""), 400)
	}
}
