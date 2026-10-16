package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func declineHistory(expected, requestID string) []harkMessage {
	done := false
	notice := harkMessage{ID: "notice", Role: "assistant", Source: "declined_notice", Content: "I wasn't able to answer this message.", Streaming: &done}
	notice.Presentation.Kind = "declined"
	return []harkMessage{
		{ID: requestID, Role: "user"},
		{ID: expected, Role: "assistant", TriggeredBy: requestID, Source: "web", JobStatus: "completed", Streaming: &done},
		notice,
	}
}

func TestDeclinedNoticeRequiresCompletedBoundTurn(t *testing.T) {
	for _, name := range []string{"completed", "bound_notice", "old_turn", "next_turn", "before_primary", "missing_user", "missing_primary", "wrong_primary", "wrong_trigger", "streaming", "unknown_streaming", "wrong_kind", "wrong_source", "wrong_role", "empty", "onboarding", "pending_primary", "pending_followup", "failed_primary"} {
		t.Run(name, func(t *testing.T) {
			messages := declineHistory("expected", "request")
			want, failed := "", false
			streaming := true
			switch name {
			case "completed":
				want = messages[2].Content
			case "bound_notice":
				messages[2].TriggeredBy = "request"
				want = messages[2].Content
			case "old_turn":
				messages = append([]harkMessage{{ID: "old", Role: "user"}, messages[2]}, messages[:2]...)
			case "next_turn":
				messages = append(messages[:2], harkMessage{ID: "next", Role: "user"}, messages[2])
			case "before_primary":
				messages[1], messages[2] = messages[2], messages[1]
			case "missing_user":
				messages = messages[1:]
			case "missing_primary":
				messages = append(messages[:1], messages[2])
			case "wrong_primary":
				messages[1].TriggeredBy = "other"
			case "wrong_trigger":
				messages[2].TriggeredBy = "other"
			case "streaming":
				messages[2].Streaming = &streaming
			case "unknown_streaming":
				messages[2].Streaming = nil
			case "wrong_kind":
				messages[2].Presentation.Kind = "text"
			case "wrong_source":
				messages[2].Source = "web"
			case "wrong_role":
				messages[2].Role = "tool"
			case "empty":
				messages[2].Content = " "
			case "onboarding":
				messages[2].Onboarding = json.RawMessage(`{"sample":true}`)
			case "pending_primary":
				messages[1].JobStatus = "running"
			case "pending_followup":
				messages = append(messages, harkMessage{ID: "followup", Role: "assistant", TriggeredBy: "request", JobStatus: "running"})
			case "failed_primary":
				messages[1].JobStatus = "failed"
				failed = true
			}
			got, err := completedProjectReply(messages, "expected", "request")
			if got.Text != want || (err != nil) != failed || want != "" && got.Outcome != "declined" {
				t.Fatalf("reply=%+v error=%v", got, err)
			}
		})
	}
}

func TestDeclinedResponsesJSONSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			s := testServer(t)
			s.accounts = []Account{{ID: "fixture", Status: "valid", Cookie: "session=fixture", Detail: "session valid"}}
			key := sessionKey(t, s)
			created, sends, polls := 0, 0, 0
			expected, requestID := "", ""
			s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := "{}"
				switch r.URL.Path {
				case "/api/conversations":
					created++
					body = `{"id":"fixture-project"}`
				case "/api/messages/send":
					sends++
					var input map[string]string
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["message"] != projectScopedMessage("fixture-project", "fixture request") {
						t.Fatal("changed input", err)
					}
					expected, requestID = input["responseMessageId"], input["idempotencyKey"]
					body = fmt.Sprintf(`{"success":true,"conversationId":"fixture-project","messageId":%q,"redirected":false}`, requestID)
				case "/api/messages/history":
					polls++
					b, _ := json.Marshal(map[string]any{"messages": declineHistory(expected, requestID)})
					body = string(b)
				default:
					t.Fatal("unexpected upstream path", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			w := call(s, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"hark","messages":[{"role":"user","content":"fixture request"}],"stream":%t}`, stream), key)
			assertStatus(t, w, 200)
			if w.Header().Get("X-Hark-Upstream-Outcome") != "declined" || created != 1 || sends != 1 || polls != 1 {
				t.Fatal("wrong outcome or repeated request", created, sends, polls, w.Header())
			}
			if s.accounts[0].ChatVerified || s.accounts[0].ChatChecked != "" || s.accounts[0].Detail != "session valid" {
				t.Fatal("decline falsely verified chat")
			}
			if e := s.events[len(s.events)-1]; e.Level != "warning" || !strings.Contains(e.Detail, "declined_notice") {
				t.Fatal("decline logged as success", e)
			}
			text, finish := "", ""
			var choices struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					Finish string `json:"finish_reason"`
				} `json:"choices"`
			}
			if stream {
				if w.Header().Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("SSE did not terminate")
				}
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: {") {
						continue
					}
					choices.Choices = nil
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &choices); err != nil || len(choices.Choices) != 1 {
						t.Fatal("invalid SSE", err)
					}
					text += choices.Choices[0].Delta.Content
					if choices.Choices[0].Finish != "" {
						finish = choices.Choices[0].Finish
					}
				}
			} else {
				if err := json.Unmarshal(w.Body.Bytes(), &choices); err != nil || len(choices.Choices) != 1 {
					t.Fatal("invalid JSON", err)
				}
				text, finish = choices.Choices[0].Message.Content, choices.Choices[0].Finish
			}
			if text != "I wasn't able to answer this message." || finish != "stop" {
				t.Fatal("upstream text/finish changed", text, finish)
			}
			fmt.Printf("DECLINED_RESPONSE stream=%t status=%d outcome=declined sends=%d polls=%d finish=stop chat_verified=false content=%q\n", stream, w.Code, sends, polls, text)
		})
	}
}

// Explicit opt-in, read-only evidence from already completed Hark requests.
// Do not resubmit the original user/agent request to reproduce a refusal.
func TestObservedDeclinedHistory(t *testing.T) {
	data := os.Getenv("HARK_PENDING_AUDIT_DATA")
	if data == "" {
		t.Skip("read-only live history audit not requested")
	}
	s, err := New(data)
	if err != nil {
		t.Fatal("read existing store failed")
	}
	if err = s.SetUpstreamProxy("http://127.0.0.1:7897"); err != nil {
		t.Fatal(err)
	}
	var projects []*projectSession
	for _, p := range s.projects {
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].LastUsed.After(projects[j].LastUsed) })
	matched := 0
	for _, p := range projects {
		cookie := ""
		for _, a := range s.accounts {
			if a.ID == p.AccountID {
				cookie = a.Cookie
				break
			}
		}
		if cookie == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var history struct {
			Messages []harkMessage `json:"messages"`
		}
		err = s.harkJSON(ctx, "GET", "/api/messages/history?conversationId="+url.QueryEscape(p.ConversationID), cookie, nil, &history)
		cancel()
		if err != nil {
			t.Fatal("read-only upstream history failed")
		}
		requestID, expected, notice := "", "", ""
		for _, m := range history.Messages {
			if m.Role == "user" {
				requestID = m.ID
				expected = ""
				notice = ""
			}
			if m.Role == "assistant" && m.TriggeredBy == requestID && m.JobStatus == "completed" && m.Source == "web" {
				expected = m.ID
			}
			if m.Source == "declined_notice" && m.Presentation.Kind == "declined" {
				notice = m.Content
			}
		}
		if expected == "" || requestID == "" || notice == "" {
			continue
		}
		text, err := completedReply(history.Messages, expected, requestID)
		if err != nil {
			t.Fatal("unexpected parser error")
		}
		// Only serialize the known public refusal notice, never history/IDs/input.
		result := map[string]any{"job_completed": true, "declined_notice_present": true, "notice_chars": len(notice), "parser_returned_notice": text == notice, "parser_pending": text == "", "message_count": len(history.Messages)}
		if text == notice {
			result["reply"] = text
		}
		b, _ := json.Marshal(result)
		fmt.Printf("READ_ONLY_HISTORY %s\n", b)
		mode := os.Getenv("HARK_PENDING_AUDIT_EXPECT")
		if mode == "baseline" && text != "" || mode == "modified" && text != notice {
			t.Fatal("observed history parser result mismatch")
		}
		matched++
		if matched == 2 {
			break
		}
	}
	if matched == 0 {
		t.Fatal("no recent completed decline history available")
	}
}
