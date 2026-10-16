package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeOpenAITextAndSDKOptions(t *testing.T) {
	input := `{"model":"hark","stream":true,"messages":[{"role":"developer","content":"rules","name":"agent"},{"role":"user","content":[{"type":"text","text":"first"},{"type":"input_text","text":"second"}]}],"temperature":0.5,"max_tokens":1024,"max_completion_tokens":2048,"top_p":1,"stream_options":{"include_usage":true},"metadata":{"client":"fixture"},"tools":[],"tool_choice":"auto","response_format":{"type":"text"},"n":1}`
	v, ignored, err := normalizeChat([]byte(input))
	if err != nil || !v.Stream || v.Messages[0].Role != "developer" || v.Messages[1].Content != "first\nsecond" {
		t.Fatal("SDK normalization failed", err, v)
	}
	if len(ignored) != 0 || string(v.OriginalBody) != input {
		t.Fatal("missing ignored options", ignored)
	}
	text := transcript(v)
	if !strings.Contains(text, `"role":"developer"`) || !strings.Contains(text, `"type":"input_text","text":"second"`) {
		t.Fatal("normalized context lost", text)
	}
}

func TestChatInputErrorsAreSpecificAndSendNothing(t *testing.T) {
	cases := []struct{ body, code string }{
		{`{"model":"hark","messages":[]}`, "invalid_request"},
		{`{"model":"hark","messages":[{"content":"hello"}]}`, "invalid_request"},
		{`{"model":"hark","messages":[{"role":"user","content":"hello"}],"conversationId":"other"}`, "reserved_gateway_field"},
		{`{"model":"hark","messages":[{"role":"user","content":123}]}`, "invalid_request"},
		{`{"model":"hark","messages":[{"role":"user","content":"hello"}],"stream":"true"}`, "invalid_request"},
		{`{"model":"other","messages":[{"role":"user","content":"hello"}]}`, "invalid_request"},
		{`{} {}`, "invalid_json"},
		{`{invalid`, "invalid_json"},
		{`null`, "invalid_json"},
		{`[]`, "invalid_json"},
	}
	s := testServer(t)
	s.accounts = []Account{{ID: "fixture", Status: "valid", Cookie: "session=fixture"}}
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { t.Fatal("invalid input reached Hark"); return nil, nil })}
	for _, tc := range cases {
		w := call(s, "POST", "/admin/playground/chat", tc.body, "")
		assertStatus(t, w, 400)
		if !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatal("wrong error", tc.body, w.Body.String())
		}
	}
}

func TestChatLimitIndependentOfManagementLimit(t *testing.T) {
	s := testServer(t)
	body := `{"model":"hark","messages":[{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}],"temperature":0.7}`
	w := call(s, "POST", "/admin/validate", body, "")
	assertStatus(t, w, 200)
	assertStatus(t, call(s, "POST", "/admin/playground/chat", body, ""), 409)
	body = `{"model":"hark","messages":[{"role":"user","content":"` + strings.Repeat("x", maxChatBody) + `"}]}`
	w = call(s, "POST", "/admin/validate", body, "")
	assertStatus(t, w, 413)
	if !strings.Contains(w.Body.String(), `"code":"request_too_large"`) {
		t.Fatal(w.Body.String())
	}
	assertStatus(t, call(s, "POST", "/admin/keys", `{"name":"`+strings.Repeat("x", 40000)+`"}`, ""), 400)
	assertStatus(t, call(s, "PUT", "/admin/settings", `{"name":"fixture","timezone":"UTC","timeout":90,"extra":true}`, ""), 400)
}

func TestDownstreamKeyNormalizationUsesProject(t *testing.T) {
	s := testServer(t)
	s.accounts = []Account{{ID: "fixture", Status: "valid", Cookie: "session=fixture"}}
	count := fixtureDirect(t, s)
	var key struct {
		Secret string `json:"secret"`
	}
	json.Unmarshal(call(s, "POST", "/admin/keys", `{"name":"fixture"}`, "").Body.Bytes(), &key)
	for _, stream := range []string{"false", "true"} {
		body := `{"model":"hark","messages":[{"role":"developer","content":"rules"},{"role":"user","content":[{"type":"text","text":"hello"}]}],"temperature":0.3,"stream_options":{"include_usage":true},"stream":` + stream + `}`
		w := call(s, "POST", "/v1/chat/completions", body, key.Secret)
		assertStatus(t, w, 200)
		if w.Header().Get("X-Hark-Input-Adapter") != "native-project-v1" || w.Header().Get("X-Hark-Ignored-Parameters") != "" || !strings.Contains(w.Body.String(), "fixture answer") {
			t.Fatal("missing adapter info or answer", w.Header(), w.Body.String())
		}
	}
	if *count != 2 {
		t.Fatal("shared Project", *count)
	}
	assertStatus(t, call(s, "POST", "/v1/chat/completions", `{invalid`, ""), 401)
}

func TestNormalizationActuallyChangesUpstreamPayload(t *testing.T) {
	v, _, err := normalizeChat([]byte(`{"model":"hark","messages":[{"role":"user","content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}],"temperature":0.8}`))
	if err != nil || transcript(v) != "alpha\nbeta" {
		t.Fatal(err, transcript(v))
	}
	r := httptest.NewRequest("POST", "/admin/validate", strings.NewReader(`{"model":"hark","messages":[{"role":"user","content":"hi"}],"sdk_extra":true}`))
	w := httptest.NewRecorder()
	if !decodeChat(w, r, &v) || w.Header().Get("X-Hark-Ignored-Parameters") != "" || !strings.Contains(string(v.OriginalBody), `"sdk_extra":true`) {
		t.Fatal(w.Body.String(), w.Header())
	}
}
