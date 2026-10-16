package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMissingAccountDoesNotSendChat(t *testing.T) {
	s := testServer(t)
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("unverified Project flow made an upstream request", r.URL.Path)
		return nil, nil
	})}
	w := call(s, "POST", "/admin/keys", `{"name":"client"}`, "")
	assertStatus(t, w, 201)
	var created struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	w = call(s, "POST", "/v1/chat/completions", `{"model":"hark","messages":[{"role":"user","content":"hello"}]}`, created.Secret)
	assertStatus(t, w, 409)
	if !strings.Contains(w.Body.String(), "no_valid_account") {
		t.Fatal("missing account not made explicit", w.Body.String())
	}
	assertStatus(t, call(s, "POST", "/admin/projects", `{"name":"manual","url":"https://hark.com/project/manual"}`, ""), 405)
	js := call(s, "GET", "/app.js", "", "").Body.String()
	if strings.Contains(js, "project-selection-form") || strings.Contains(js, "extension-status") || !strings.Contains(js, "account-form") {
		t.Fatal("manual Project or extension UI remained")
	}
}
