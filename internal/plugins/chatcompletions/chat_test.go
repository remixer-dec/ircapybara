package chatcompletions

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChatCompletionRequestShape(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"actual-model-2026","choices":[{"message":{"content":"hello from model"}}]}`))
	}))
	defer ts.Close()

	p := &Plugin{endpoint: ts.URL + "/v1/chat/completions", model: "test-model", reasoning: "low", http: &http.Client{Timeout: time.Second}}
	answer, responseModel, err := p.request([]message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "hello from model" {
		t.Fatalf("answer=%q", answer)
	}
	if responseModel != "actual-model-2026" {
		t.Fatalf("response model=%q", responseModel)
	}
	if got["model"] != "test-model" || got["reasoning_effort"] != "low" {
		t.Fatalf("unexpected request: %#v", got)
	}
	if _, ok := got["tools"]; ok {
		t.Fatal("simple chat plugin must not send tools")
	}
}

func TestModelSenderNickUsesResponseModelSafely(t *testing.T) {
	cases := map[string]string{
		"gpt-5.6-sol":            "gpt-5-6-sol",
		"provider/model:version": "provider-model-version",
		"2026-model":             "ai-2026-model",
	}
	for input, want := range cases {
		if got := modelSenderNick(input, "chat-ai"); got != want {
			t.Errorf("modelSenderNick(%q)=%q want %q", input, got, want)
		}
	}
	if got := modelSenderNick("", "chat-ai"); got != "chat-ai" {
		t.Fatalf("empty response model should use fallback, got %q", got)
	}
}
