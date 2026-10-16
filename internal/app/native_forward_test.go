package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestNativeForwardPreservesValuesWithinProjectBoundary(t *testing.T) {
	input := `{"model":"hark","session_id":"one","stream":true,"messages":[{"role":"developer","content":"original rules","name":"agent"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_original","type":"function","function":{"name":"f","arguments":"{\"number\":9007199254740993}"}}]},{"role":"tool","tool_call_id":"call_original","content":"result"}],"tools":[{"type":"function","function":{"name":"f","strict":true,"parameters":{"$ref":"https://example.test/schema"}}}],"tool_choice":{"type":"function","function":{"name":"f"}},"parallel_tool_calls":false,"temperature":0.3,"n":2,"response_format":{"type":"json_schema","json_schema":{"name":"x"}},"custom":{"number":9007199254740993,"str":"<unchanged>"}}`
	v, ignored, err := normalizeChat([]byte(input))
	if err != nil || len(ignored) > 0 || v.Messages[0].Role != "developer" {
		t.Fatal("locally rewritten/rejected options", err)
	}
	payload, err := nativeProjectPayload(v, "UTC", "fixture-project")
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]json.RawMessage
	json.Unmarshal([]byte(input), &original)
	for name, raw := range original {
		switch name {
		case "model", "messages", "stream", "session_id", "session_end":
			continue
		}
		forwarded, ok := payload[name].(json.RawMessage)
		if !ok || !reflect.DeepEqual(raw, forwarded) {
			t.Fatal("original field changed", name)
		}
	}
	if payload["message"] != projectScopedMessage("fixture-project", string(original["messages"])) || payload["conversationId"] != "fixture-project" || payload["timezone"] != "UTC" {
		t.Fatal("routing/message conversion wrong")
	}
	for _, name := range []string{"model", "messages", "stream", "session_id", "session_end"} {
		if _, ok := payload[name]; ok {
			t.Fatal("gateway field leaked", name)
		}
	}
	if strings.Contains(payload["message"].(string), "HARK2API DOWNSTREAM") || strings.Contains(payload["message"].(string), "以下是客户端") {
		t.Fatal("injected instruction")
	}
	input = `{"model":"hark","messages":[{"role":"user","content":" exact text\nsecond line "}],"timezone":"Asia/Tokyo"}`
	v, _, err = normalizeChat([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = nativeProjectPayload(v, "UTC", "fixture-project")
	if payload["message"] != projectScopedMessage("fixture-project", " exact text\nsecond line ") || string(payload["timezone"].(json.RawMessage)) != `"Asia/Tokyo"` {
		t.Fatal("user text/timezone altered")
	}
}

func TestNativeGatewayForwardsToolsAndHonorsHarkRejection(t *testing.T) {
	s := testServer(t)
	s.accounts = []Account{{ID: "fixture", Name: "fixture", Status: "valid", Cookie: "session=fixture"}}
	key := sessionKey(t, s)
	created, sends, histories := 0, 0, 0
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "session=fixture" || r.Header.Get("Authorization") != "" {
			t.Fatal("wrong upstream auth")
		}
		body, status := "{}", 200
		switch r.URL.Path {
		case "/api/conversations":
			created++
			body = `{"id":"fixture-project"}`
		case "/api/messages/send":
			sends++
			var data map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&data)
			var message string
			json.Unmarshal(data["message"], &message)
			if string(data["tool_choice"]) != `"required"` || string(data["tools"]) != `[{"type":"function","function":{"name":"f"}}]` || message != projectScopedMessage("fixture-project", "hello") {
				t.Fatal("tools transformed instead of forwarded", data)
			}
			status, body = 403, `{"error":"run override not enabled"}`
		case "/api/messages/history":
			histories++
			t.Fatal("rejected send polled")
		default:
			t.Fatal("unexpected route")
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	w := call(s, "POST", "/v1/chat/completions", `{"model":"hark","session_id":"one","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"required"}`, key)
	assertStatus(t, w, 403)
	if !strings.Contains(w.Body.String(), `"code":"upstream_run_override_denied"`) || !strings.Contains(w.Body.String(), "run override not enabled") || created != 1 || sends != 1 || histories != 0 {
		t.Fatal("concealed failure/retried", w.Body.String())
	}
	if len(s.projects) != 1 {
		t.Fatal("failed send discarded mapping")
	}
}

func TestNativeResponsesAreLiteralJSONAndSSE(t *testing.T) {
	// A JSON-looking text reply is content, never evidence of native tool calls.
	text := `{"content":null,"tool_calls":[{"name":"f","arguments":{"x":1}}]}`
	for _, stream := range []bool{false, true} {
		s := testServer(t)
		s.accounts = []Account{{ID: "fixture", Name: "fixture", Status: "valid", Cookie: "session=fixture"}}
		key := sessionKey(t, s)
		var expected, requestID string
		s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := "{}"
			switch r.URL.Path {
			case "/api/conversations":
				body = `{"id":"fixture-project"}`
			case "/api/messages/send":
				var input map[string]string
				json.NewDecoder(r.Body).Decode(&input)
				expected, requestID = input["responseMessageId"], input["idempotencyKey"]
				body = fmt.Sprintf(`{"success":true,"conversationId":"fixture-project","messageId":%q,"redirected":false}`, requestID)
			case "/api/messages/history":
				body = fmt.Sprintf(`{"messages":[{"id":%q,"role":"assistant","triggeredByMessageId":%q,"content":%q,"jobStatus":"completed","isStreaming":false}]}`, expected, requestID, text)
			default:
				t.Fatal("unexpected route")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		w := call(s, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"hark","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream), key)
		assertStatus(t, w, 200)
		var reply string
		if stream {
			if !strings.Contains(w.Body.String(), "data: [DONE]") {
				t.Fatal("missing SSE end")
			}
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var chunk struct {
					Choices []struct {
						Delta  map[string]any `json:"delta"`
						Finish *string        `json:"finish_reason"`
					} `json:"choices"`
				}
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk)
				if _, ok := chunk.Choices[0].Delta["tool_calls"]; ok {
					t.Fatal("text converted to fake calls")
				}
				if c, ok := chunk.Choices[0].Delta["content"].(string); ok {
					reply += c
				}
				if f := chunk.Choices[0].Finish; f != nil && *f != "stop" {
					t.Fatal("wrong finish")
				}
			}
		} else {
			var answer struct {
				Choices []struct {
					Message map[string]any `json:"message"`
					Finish  string         `json:"finish_reason"`
				} `json:"choices"`
			}
			json.Unmarshal(w.Body.Bytes(), &answer)
			if _, ok := answer.Choices[0].Message["tool_calls"]; ok || answer.Choices[0].Finish != "stop" {
				t.Fatal("fake native tools")
			}
			reply = answer.Choices[0].Message["content"].(string)
		}
		if reply != text || w.Header().Get("X-Hark-Tool-Adapter") != "" {
			t.Fatal("Hark reply changed")
		}
	}
}
