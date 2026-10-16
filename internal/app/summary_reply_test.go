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
	"unicode/utf8"
)

// Read-only, explicit opt-in: audit already completed upstream replies.
func TestObservedSummaryHistory(t *testing.T) {
	data := os.Getenv("HARK_SUMMARY_AUDIT_DATA")
	if data == "" {
		t.Skip("read-only history audit not requested")
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
		requestID, expected := "", ""
		for _, m := range history.Messages {
			if m.Role == "user" {
				requestID = m.ID
				expected = ""
			}
			if m.Role == "assistant" && m.TriggeredBy == requestID && m.Source == "web" && m.JobStatus == "completed" {
				expected = m.ID
			}
		}
		if requestID == "" || expected == "" {
			continue
		}
		textParts, summaryParts := []string{}, []string{}
		for _, m := range history.Messages {
			if m.Role != "assistant" || m.TriggeredBy != requestID || m.Source != "send_bubble" || m.Streaming == nil || *m.Streaming || m.Content == "" {
				continue
			}
			switch m.Presentation.Kind {
			case "text":
				textParts = append(textParts, m.Content)
			case "summary":
				summaryParts = append(summaryParts, m.Content)
			}
		}
		if len(summaryParts) == 0 {
			continue
		}
		text, err := completedReply(history.Messages, expected, requestID)
		if err != nil {
			t.Fatal("history parser failed")
		}
		fullParts := []string{}
		for _, m := range history.Messages {
			if m.Role == "assistant" && m.TriggeredBy == requestID && m.Source == "send_bubble" && (m.Presentation.Kind == "text" || m.Presentation.Kind == "summary") && m.Content != "" {
				fullParts = append(fullParts, m.Content)
			}
		}
		full := strings.Join(fullParts, "\n\n")
		result := map[string]any{"text_chars": utf8.RuneCountInString(strings.Join(textParts, "\n\n")), "summary_chars": utf8.RuneCountInString(strings.Join(summaryParts, "\n\n")), "returned_chars": utf8.RuneCountInString(text), "all_content_returned": text == full, "summary_is_json": json.Valid([]byte(summaryParts[0]))}
		b, _ := json.Marshal(result)
		fmt.Printf("READ_ONLY_SUMMARY %s\n", b)
		mode := os.Getenv("HARK_SUMMARY_AUDIT_EXPECT")
		if mode == "baseline" && text != strings.Join(textParts, "\n\n") || mode == "modified" && text != full {
			t.Fatal("observed parser result mismatch")
		}
		matched++
		if matched == 3 {
			break
		}
	}
	if matched == 0 {
		t.Fatal("no recent summary reply history available")
	}
}

func TestSummaryReplyCorrelationAndCompletion(t *testing.T) {
	done, streaming := false, true
	job := harkMessage{ID: "expected", Role: "assistant", TriggeredBy: "request", JobStatus: "completed", Streaming: &done}
	summary := harkMessage{ID: "summary", Role: "assistant", TriggeredBy: "request", Source: "send_bubble", Content: "## Full answer\n\nDetailed content.", Streaming: &done}
	summary.Presentation.Kind = "summary"
	for _, name := range []string{"completed", "wrong_request", "unbound", "streaming", "pending_job", "missing_job", "unknown_completion", "onboarding", "unknown_kind"} {
		t.Run(name, func(t *testing.T) {
			messages := []harkMessage{job, summary}
			want, failed := "", false
			switch name {
			case "completed":
				want = summary.Content
			case "wrong_request":
				messages[1].TriggeredBy = "other"
			case "unbound":
				messages[1].TriggeredBy = ""
			case "streaming":
				messages[1].Streaming = &streaming
			case "pending_job":
				messages[0].JobStatus = "running"
			case "missing_job":
				messages = messages[1:]
			case "unknown_completion":
				messages[1].Streaming = nil
				failed = true
			case "onboarding":
				messages[1].Onboarding = json.RawMessage(`{"fixture":true}`)
			case "unknown_kind":
				messages[1].Presentation.Kind = "image"
			}
			text, err := completedReply(messages, "expected", "request")
			if text != want || (err != nil) != failed {
				t.Fatal(text, err)
			}
		})
	}
}

func TestSummaryResponsesJSONSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			s := testServer(t)
			s.accounts = []Account{{ID: "fixture", Name: "fixture", Status: "valid", Cookie: "session=fixture"}}
			key := sessionKey(t, s)
			fixtureDirect(t, s)
			base := s.client.Transport
			summaryText := "## Full answer\n\n" + strings.Repeat("Detailed content.\n", 60)
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := base.RoundTrip(r)
				if err != nil || r.URL.Path != "/api/messages/history" {
					return resp, err
				}
				defer resp.Body.Close()
				var history struct {
					Messages []harkMessage `json:"messages"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
					t.Fatal(err)
				}
				bubble := history.Messages[len(history.Messages)-1]
				bubble.ID = "summary"
				bubble.Content = summaryText
				bubble.Presentation.Kind = "summary"
				history.Messages = append(history.Messages, bubble)
				b, _ := json.Marshal(history)
				resp.Body = io.NopCloser(strings.NewReader(string(b)))
				return resp, nil
			})
			w := call(s, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"hark","messages":[{"role":"user","content":"fixture"}],"stream":%t}`, stream), key)
			assertStatus(t, w, 200)
			text, finish := "", ""
			if stream {
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: {") {
						continue
					}
					var chunk struct {
						Choices []struct {
							Delta struct {
								Content string `json:"content"`
							} `json:"delta"`
							Finish *string `json:"finish_reason"`
						} `json:"choices"`
					}
					if json.Unmarshal([]byte(line[6:]), &chunk) != nil || len(chunk.Choices) != 1 {
						t.Fatal("invalid SSE")
					}
					text += chunk.Choices[0].Delta.Content
					if chunk.Choices[0].Finish != nil {
						finish = *chunk.Choices[0].Finish
					}
				}
				if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("SSE not terminated")
				}
			} else {
				var answer struct {
					Choices []struct {
						Message chatMessage `json:"message"`
						Finish  string      `json:"finish_reason"`
					} `json:"choices"`
				}
				if json.Unmarshal(w.Body.Bytes(), &answer) != nil || len(answer.Choices) != 1 {
					t.Fatal("invalid JSON")
				}
				text, finish = answer.Choices[0].Message.Content, answer.Choices[0].Finish
			}
			if text != "fixture answer\n\n"+summaryText || finish != "stop" {
				t.Fatal("summary lost or altered")
			}
			fmt.Printf("SUMMARY_RESPONSE stream=%t status=200 returned_chars=%d finish=stop\n", stream, utf8.RuneCountInString(text))
		})
	}
}
