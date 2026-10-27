package analyze

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestAnalyzeDriveRequest(t *testing.T) {
	var got map[string]any
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		beta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"thinking","thinking":"","signature":"x"},{"type":"text","text":"### Health summary\nAll good."}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()

	c := NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	s := summarize(1, time.Unix(1_700_000_000, 0), healthy)
	text, err := c.AnalyzeDrive(context.Background(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text != "### Health summary\nAll good." {
		t.Errorf("text = %q", text)
	}
	if got["model"] != DefaultModel || got["fallbacks"] != "default" {
		t.Errorf("model/fallbacks: %v / %v", got["model"], got["fallbacks"])
	}
	if !strings.Contains(beta, "server-side-fallback-2026-07-01") {
		t.Errorf("beta header %q", beta)
	}
	msgs, _ := json.Marshal(got["messages"])
	for _, want := range []string{"drive_data", "baseline_deltas", "warm_up"} {
		if !strings.Contains(string(msgs), want) {
			t.Errorf("request missing %q", want)
		}
	}
	if strings.Contains(string(msgs), `"samples"`) {
		t.Error("raw samples must not be sent")
	}
}

func TestAnalyzeDriveRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],
			"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":""},
			"usage":{"input_tokens":10,"output_tokens":0}}`)
	}))
	defer srv.Close()
	c := NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	if _, err := c.AnalyzeDrive(context.Background(), Summary{}, nil); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("err = %v", err)
	}
}
