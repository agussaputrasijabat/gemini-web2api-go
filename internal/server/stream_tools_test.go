package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// parseSSEPayloads extracts the JSON objects from an SSE body, skipping the
// [DONE] sentinel. Returns them in wire order.
func parseSSEPayloads(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || strings.HasPrefix(block, "event:") {
			continue
		}
		line := strings.TrimPrefix(block, "data: ")
		if line == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid SSE chunk %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestStreamerToolCallsChunkShape(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEChatStreamer(rec, "chatcmpl-test", "gemini-3.6-flash")

	if err := s.Role(); err != nil {
		t.Fatalf("Role: %v", err)
	}

	calls := []models.OpenAIToolCall{
		{
			ID:       "call_first",
			Type:     "function",
			Function: models.OpenAIToolCallFunction{Name: "get_weather", Arguments: `{"city":"Jakarta"}`},
		},
		{
			ID:       "call_second",
			Type:     "function",
			Function: models.OpenAIToolCallFunction{Name: "get_time", Arguments: `{"zone":"Asia/Jakarta"}`},
		},
	}
	if err := s.ToolCalls(calls); err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}

	payloads := parseSSEPayloads(t, rec.Body.String())
	// role chunk + 2 tool call chunks + terminal finish chunk
	if len(payloads) != 4 {
		t.Fatalf("expected 4 chunks, got %d: %v", len(payloads), payloads)
	}

	// First chunk: role.
	first := payloads[0]
	if first["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v", first["object"])
	}
	choices := first["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Errorf("role delta = %v", delta)
	}

	// Tool call chunks must carry index/id/type/function{name,arguments}.
	seenIndex := map[float64]bool{}
	seenID := map[string]bool{}
	for _, p := range payloads[1:3] {
		ch := p["choices"].([]any)[0].(map[string]any)
		d := ch["delta"].(map[string]any)
		tcs, ok := d["tool_calls"].([]any)
		if !ok || len(tcs) != 1 {
			t.Fatalf("delta.tool_calls missing: %v", d)
		}
		tc := tcs[0].(map[string]any)

		idx, ok := tc["index"].(float64)
		if !ok {
			t.Fatalf("tool call missing numeric index: %v", tc)
		}
		if seenIndex[idx] {
			t.Errorf("duplicate index %v", idx)
		}
		seenIndex[idx] = true

		id, _ := tc["id"].(string)
		if id == "" {
			t.Error("tool call missing id")
		}
		if seenID[id] {
			t.Errorf("duplicate id %q", id)
		}
		seenID[id] = true

		if tc["type"] != "function" {
			t.Errorf("type = %v", tc["type"])
		}
		fn, ok := tc["function"].(map[string]any)
		if !ok {
			t.Fatalf("function missing: %v", tc)
		}
		if fn["name"] == "" || fn["name"] == nil {
			t.Error("function.name missing")
		}
		args, _ := fn["arguments"].(string)
		if args == "" {
			t.Error("function.arguments missing")
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(args), &parsed); err != nil {
			t.Errorf("function.arguments is not valid JSON: %v (%q)", err, args)
		}
	}

	// Terminal chunk: finish_reason must be tool_calls, not stop.
	last := payloads[3]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", last["finish_reason"])
	}
}

func TestStreamerFinishStopWhenNoCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEChatStreamer(rec, "chatcmpl-x", "m")
	if err := s.Finish(nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	payloads := parseSSEPayloads(t, rec.Body.String())
	if len(payloads) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(payloads))
	}
	ch := payloads[0]["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", ch["finish_reason"])
	}
}

func TestStreamerContentDeltas(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEChatStreamer(rec, "chatcmpl-y", "m")
	_ = s.Content("Hello")
	_ = s.Content(" world")

	payloads := parseSSEPayloads(t, rec.Body.String())
	if len(payloads) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(payloads))
	}
	var got strings.Builder
	for _, p := range payloads {
		d := p["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if c, ok := d["content"].(string); ok {
			got.WriteString(c)
		}
	}
	if got.String() != "Hello world" {
		t.Errorf("content = %q", got.String())
	}
}

func TestStreamGatePassesProseThrough(t *testing.T) {
	gate := newToolStreamGate(true)
	var out strings.Builder
	emit := func(s string) error { out.WriteString(s); return nil }

	for _, d := range []string{"The wea", "ther is ", "sunny."} {
		if err := gate.Feed(d, emit); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	if got := gate.Pending(); got != "" {
		t.Errorf("pending = %q, want empty", got)
	}
	if got := out.String(); got != "The weather is sunny." {
		t.Errorf("streamed = %q", got)
	}
}

func TestStreamGateBuffersToolBlock(t *testing.T) {
	gate := newToolStreamGate(true)
	var out strings.Builder
	emit := func(s string) error { out.WriteString(s); return nil }

	chunks := []string{
		"Let me check.\n",
		"```tool_call\n",
		`{"name": "get_weather", `,
		`"arguments": {"city": "Jakarta"}}`,
		"\n```",
	}
	for _, c := range chunks {
		if err := gate.Feed(c, emit); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}

	if got := out.String(); !strings.Contains(got, "Let me check.") {
		t.Errorf("prose before the fence should stream, got %q", got)
	}
	if strings.Contains(out.String(), "tool_call") {
		t.Errorf("fence boundaries leaked into the live stream: %q", out.String())
	}

	pending := gate.Pending()
	if !strings.Contains(pending, "get_weather") {
		t.Errorf("pending = %q, want the buffered call", pending)
	}
}

func TestStreamGateSplitsMarkerAcrossDeltas(t *testing.T) {
	gate := newToolStreamGate(true)
	var out strings.Builder
	emit := func(s string) error { out.WriteString(s); return nil }

	// Split inside the backtick fence prefix; a naive matcher would stream the
	// partial fence and make the block unparseable.
	parts := []string{"Answer.\n``", "`tool_", `call {"name":"a","arguments":{}}`, "\n```"}
	for _, p := range parts {
		if err := gate.Feed(p, emit); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}

	if strings.Contains(out.String(), "```") {
		t.Errorf("partial fence leaked: %q", out.String())
	}
	if !strings.Contains(gate.Pending(), `"name":"a"`) {
		t.Errorf("pending = %q", gate.Pending())
	}
}

func TestStreamGateDisabledStreamsEverything(t *testing.T) {
	gate := newToolStreamGate(false)
	var out strings.Builder
	emit := func(s string) error { out.WriteString(s); return nil }

	body := "```tool_call\n{\"name\": \"x\", \"arguments\": {}}\n```"
	if err := gate.Feed(body, emit); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if out.String() != body {
		t.Errorf("streamed = %q, want passthrough", out.String())
	}
	if gate.Pending() != "" {
		t.Errorf("pending = %q, want empty", gate.Pending())
	}
}

func TestStreamGateFalsePositiveFlushesOnPending(t *testing.T) {
	// The model explains the tool syntax instead of using it. The marker starts
	// buffering, and Pending returns the text so the caller can emit it as
	// ordinary content when parsing finds no call.
	gate := newToolStreamGate(true)
	var out strings.Builder
	emit := func(s string) error { out.WriteString(s); return nil }

	body := "You can use a ```tool_call block to invoke a tool, but not here."
	_ = gate.Feed(body, emit)
	if p := gate.Pending(); p == "" {
		t.Fatal("expected buffered content once a tool marker appears")
	}
	if p := gate.Pending(); !strings.Contains(p, "invoke a tool") {
		t.Errorf("pending = %q, want the trailing prose", p)
	}
}
