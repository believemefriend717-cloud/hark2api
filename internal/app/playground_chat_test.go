package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaygroundUsesAdminSessionAndSameProjectAdapter(t *testing.T) {
	s := authServer(t)
	s.accounts = []Account{{ID: "fixture", Status: "valid", Cookie: "session=fixture"}}
	count := fixtureDirect(t, s)
	body := `{"model":"hark","messages":[{"role":"user","content":"fixture"}]}`
	assertStatus(t, remoteCall(s, "/admin/playground/chat", body, nil, ""), 401)
	login := remoteCall(s, "/admin/auth/login", `{"key":"fixture-admin-key"}`, nil, "")
	assertStatus(t, login, 200)
	cookie := login.Result().Cookies()[0]
	assertStatus(t, remoteCall(s, "/admin/playground/chat", body, cookie, "https://other.example"), 403)
	r := httptest.NewRequest("POST", s.publicOrigin+"/admin/playground/chat", strings.NewReader(body))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	assertStatus(t, w, 403)
	for _, stream := range []string{"false", "true"} {
		w := remoteCall(s, "/admin/playground/chat", strings.TrimSuffix(body, "}")+`,"stream":`+stream+`}`, cookie, s.publicOrigin)
		assertStatus(t, w, 200)
		if !strings.Contains(w.Body.String(), "fixture answer") || w.Header().Get("X-Hark-Session-Mode") != "project" {
			t.Fatal("wrong Project response")
		}
	}
	if *count != 2 || len(s.keys) != 0 {
		t.Fatal("shared Project or created client Key")
	}
	assertStatus(t, remoteCall(s, "/admin/playground/chat", `{"model":"wrong","messages":[]}`, cookie, ""), 400)
	assertStatus(t, remoteCall(s, "/v1/chat/completions", body, cookie, ""), http.StatusUnauthorized)
}
