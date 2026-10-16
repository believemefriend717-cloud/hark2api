package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Shared route-level reproduction also compiles against the old source.
func TestProjectInstructionBoundary(t *testing.T) {
	baseline := os.Getenv("HARK_ISOLATION_EXPECT") == "baseline"
	for _, path := range []string{"/v1/chat/completions", "/admin/playground/chat"} {
		t.Run(path, func(t *testing.T) {
			s := testServer(t)
			s.accounts = []Account{{ID: "fixture", Name: "fixture", Status: "valid", Cookie: "session=fixture"}}
			key := sessionKey(t, s)
			fixtureDirect(t, s)
			base := s.client.Transport
			sends, constrained := 0, 0
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/messages/send" {
					sends++
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					var data struct {
						Message string          `json:"message"`
						CID     string          `json:"conversationId"`
						Tools   json.RawMessage `json:"tools"`
						Custom  json.RawMessage `json:"custom"`
					}
					if json.Unmarshal(body, &data) != nil {
						t.Fatal("invalid payload")
					}
					if strings.Contains(data.Message, "HARK2API PROJECT CONTEXT BOUNDARY") {
						constrained++
						for _, needle := range []string{"Do not list, search, read, open", "account-wide", "Current Project ID: " + data.CID, "Client request:\n exact input\nnext line ", "Keep this Project context boundary"} {
							if !strings.Contains(data.Message, needle) {
								t.Fatal("missing instruction or changed input", needle)
							}
						}
					} else if data.Message != " exact input\nnext line " {
						t.Fatal("unexplained payload change")
					}
					if string(data.Tools) != "[]" || string(data.Custom) != `{"n":9007199254740993}` {
						t.Fatal("options changed")
					}
				}
				return base.RoundTrip(r)
			})
			for _, stream := range []bool{false, true} {
				body := fmt.Sprintf(`{"model":"hark","messages":[{"role":"user","content":" exact input\nnext line "}],"stream":%t,"tools":[],"custom":{"n":9007199254740993}}`, stream)
				w := call(s, "POST", path, body, key)
				assertStatus(t, w, 200)
				if !strings.Contains(w.Body.String(), "fixture answer") {
					t.Fatal("reply altered")
				}
				if !baseline && w.Header().Get("X-Hark-Context-Isolation") != "instruction-only" {
					t.Fatal("boundary not exposed")
				}
				if stream && !strings.Contains(w.Body.String(), "data: [DONE]") {
					t.Fatal("SSE not complete")
				}
			}
			want := 2
			if baseline {
				want = 0
			}
			if sends != 2 || constrained != want {
				t.Fatal("missing/duplicate send boundary", sends, constrained)
			}
			fmt.Printf("PROJECT_BOUNDARY path=%s sends=%d constrained=%d json_sse=200\n", path, sends, constrained)
		})
	}
}

func TestProjectBoundaryOnReusedSessions(t *testing.T) {
	s := testServer(t)
	f := fixtureSessions(s)
	key := sessionKey(t, s)
	for _, id := range []string{"one", "one", "two"} {
		w := sessionCall(s, key, id, false)
		assertStatus(t, w, 200)
		if w.Header().Get("X-Hark-Context-Isolation") != "instruction-only" {
			t.Fatal("missing boundary header")
		}
	}
	if f.created != 2 || f.sends != 3 {
		t.Fatal("session reuse changed")
	}
	for i, text := range f.messages {
		cid := "c1"
		if i == 2 {
			cid = "c2"
		}
		if text != projectScopedMessage(cid, "hello") || strings.Count(text, projectContextBoundary) != 1 {
			t.Fatal("missing/repeated boundary or changed session text")
		}
	}
}
