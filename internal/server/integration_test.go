package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// stubGeminiRequester stands in for the Gemini web RPC endpoint. The live
// endpoint was IP-blocked during verification, so these tests exercise the full
// handler -> prompt -> upstream parse -> SSE emit path against a canned upstream
// body instead. The body is shaped exactly like a real StreamGenerate response.
type stubGeminiRequester struct {
	// responses holds progressively growing answer texts, one per line. The
	// stream parser turns each into a delta.
	responses []string
	calls     int
}

func (s *stubGeminiRequester) Do(req *http.Request) (*http.Response, error) {
	s.calls++
	// Trailing newline matters: StreamParser only processes complete lines, so
	// a final snapshot without one would never be emitted. The real endpoint
	// always terminates with a newline.
	body := strings.Join(s.responses, "\n") + "\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// geminiLine renders one `wrb.fr` line carrying one answer snapshot. Each
// snapshot is cumulative, exactly like the real StreamGenerate endpoint: the
// parser diffs consecutive snapshots into deltas.
func geminiLine(t *testing.T, text string) string {
	t.Helper()
	inner := make([]any, 5)
	inner[4] = []any{
		[]any{nil, []any{text}},
	}
	// The real payload carries a long tail of metadata; ExtractTextsFromLine
	// ignores lines shorter than 200 bytes, so pad the inner JSON to a
	// realistic length.
	for len(inner) < 5 || lenMust(t, inner) < 160 {
		inner = append(inner, strings.Repeat("padding ", 20))
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	outer := []any{
		[]any{"wrb.fr", nil, string(innerJSON), nil, nil, nil, "generic"},
	}
	outerJSON, err := json.Marshal(outer)
	if err != nil {
		t.Fatalf("marshal outer: %v", err)
	}
	return string(outerJSON)
}

// lenMust returns the marshaled length of v, or a large number on error.
func lenMust(t *testing.T, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 1 << 30
	}
	return len(b)
}

// newStubbedApp builds an App whose Gemini client is backed by the stub.
// Each text is sent as one cumulative snapshot.
func newStubbedApp(t *testing.T, texts []string) (*App, *stubGeminiRequester) {
	t.Helper()
	cfg := config.Default()
	cfg.LogRequests = false
	cfg.RetryAttempts = 1
	app := New(cfg, "test-stub")

	stub := &stubGeminiRequester{}
	for _, text := range texts {
		stub.responses = append(stub.responses, geminiLine(t, text))
	}
	app.Gem.HTTP = stub
	return app, stub
}

func TestChatStreamEmitsToolCallChunks(t *testing.T) {
	// The model answers with prose then a tool_call block, streamed as three
	// progressively longer snapshots.
	final := "Let me look that up.\n```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n```"
	prefixOne := "Let me look that up."
	prefixTwo := "Let me look that up.\n```tool_call\n{\"name\": \"get_weather\", \"arg"

	app, stub := newStubbedApp(t, []string{prefixOne, prefixTwo, final})
	body := `{
		"model": "gemini-3.6-flash",
		"stream": true,
		"messages": [{"role": "user", "content": "weather in Jakarta?"}],
		"tools": [{"type":"function","function":{
			"name":"get_weather",
			"description":"Get current weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
		}}],
		"tool_choice": "required"
	}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if stub.calls == 0 {
		t.Fatal("stub upstream was never called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	payloads := parseSSEPayloads(t, rec.Body.String())
	if len(payloads) < 3 {
		t.Fatalf("expected role + tool_calls + finish chunks, got %d: %v", len(payloads), payloads)
	}

	var sawToolCall bool
	var sawToolFinish bool
	var streamedText strings.Builder

	for _, p := range payloads {
		choices, _ := p["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch := choices[0].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		if delta != nil {
			if c, ok := delta["content"].(string); ok {
				streamedText.WriteString(c)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
				sawToolCall = true
				tc := tcs[0].(map[string]any)
				if _, ok := tc["index"].(float64); !ok {
					t.Errorf("tool_calls entry missing index: %v", tc)
				}
				if id, _ := tc["id"].(string); id == "" {
					t.Errorf("tool_calls entry missing id: %v", tc)
				}
				if tc["type"] != "function" {
					t.Errorf("tool_calls type = %v", tc["type"])
				}
				fn, _ := tc["function"].(map[string]any)
				if fn == nil || fn["name"] != "get_weather" {
					t.Errorf("function = %v", fn)
				}
				rawArgs, _ := fn["arguments"].(string)
				var args map[string]any
				if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
					t.Errorf("arguments not JSON: %v (%q)", err, rawArgs)
				} else if args["city"] != "Jakarta" {
					t.Errorf("arguments = %v", args)
				}
			}
		}
		if ch["finish_reason"] == "tool_calls" {
			sawToolFinish = true
		}
	}

	if !sawToolCall {
		t.Errorf("no delta.tool_calls chunk in stream:\n%s", rec.Body.String())
	}
	if !sawToolFinish {
		t.Errorf("no finish_reason=tool_calls chunk in stream:\n%s", rec.Body.String())
	}
	// The raw tool syntax must never reach the client as content.
	if strings.Contains(streamedText.String(), "tool_call") || strings.Contains(streamedText.String(), "```") {
		t.Errorf("raw tool syntax leaked into content: %q", streamedText.String())
	}
	// Leading prose should still be streamed.
	if !strings.Contains(streamedText.String(), "Let me look that up.") {
		t.Errorf("prose lost: %q", streamedText.String())
	}
}

func TestChatStreamPlainTextHasStopFinish(t *testing.T) {
	app, _ := newStubbedApp(t, []string{"Hello", "Hello there", "Hello there friend"})
	body := `{"model":"gemini-3.6-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	payloads := parseSSEPayloads(t, rec.Body.String())
	if len(payloads) < 2 {
		t.Fatalf("expected several chunks, got %d", len(payloads))
	}
	last := payloads[len(payloads)-1]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", last["finish_reason"])
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("stream should end with [DONE]")
	}
}

func TestChatNonStreamToolCallShape(t *testing.T) {
	final := "```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Bandung\"}}\n```"
	app, _ := newStubbedApp(t, []string{final})
	body := `{
		"model":"gemini-3.6-flash",
		"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":"auto"
	}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp models.OpenAIChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
	ch := resp.Choices[0]
	if ch.FinishReason == nil || *ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v", ch.FinishReason)
	}
	if ch.Message == nil || len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("message.tool_calls = %v", ch.Message)
	}
	tc := ch.Message.ToolCalls[0]
	if tc.Function.Name != "get_weather" {
		t.Errorf("name = %q", tc.Function.Name)
	}
	if tc.ID == "" {
		t.Error("tool call id is empty")
	}
	if tc.Type != "function" {
		t.Errorf("type = %q", tc.Type)
	}
	// content must be null, not an empty string, for a tool-call-only turn.
	if ch.Message.Content != nil {
		t.Errorf("content = %v, want null", ch.Message.Content)
	}
}

func TestResponsesStreamFunctionCallEvents(t *testing.T) {
	final := "```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n```"
	app, _ := newStubbedApp(t, []string{final})
	body := `{
		"model":"gemini-3.6-flash",
		"stream":true,
		"input":"weather in Jakarta?",
		"tools":[{"type":"function","name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
	}`

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// Responses SSE carries `event:` lines; collect the type of each event.
	var types []string
	var argDeltas []string
	var addedItems []map[string]any
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		if dataLine == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(dataLine), &ev); err != nil {
			t.Fatalf("bad event JSON: %v (%q)", err, dataLine)
		}
		evType, _ := ev["type"].(string)
		types = append(types, evType)

		switch evType {
		case "response.output_item.added":
			item, _ := ev["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				addedItems = append(addedItems, item)
			}
		case "response.function_call_arguments.delta":
			if d, ok := ev["delta"].(string); ok {
				argDeltas = append(argDeltas, d)
			}
		}
	}

	if len(types) == 0 {
		t.Fatal("no SSE events emitted")
	}
	if types[0] != "response.created" {
		t.Errorf("first event = %q, want response.created", types[0])
	}
	if types[len(types)-1] != "response.completed" {
		t.Errorf("last event = %q, want response.completed", types[len(types)-1])
	}
	if len(addedItems) != 1 {
		t.Fatalf("expected 1 function_call item, got %d", len(addedItems))
	}
	item := addedItems[0]
	if item["name"] != "get_weather" {
		t.Errorf("item.name = %v", item["name"])
	}
	if item["call_id"] == nil || item["call_id"] == "" {
		t.Error("item.call_id missing")
	}
	if item["id"] == item["call_id"] {
		t.Errorf("item.id and call_id must differ, both %v", item["id"])
	}

	joined := strings.Join(argDeltas, "")
	var args map[string]any
	if err := json.Unmarshal([]byte(joined), &args); err != nil {
		t.Fatalf("accumulated argument deltas are not JSON: %v (%q)", err, joined)
	}
	if args["city"] != "Jakarta" {
		t.Errorf("accumulated args = %v", args)
	}

	// Sequence numbers must be strictly increasing so clients can order events.
	seen := map[float64]bool{}
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		var dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		if dataLine == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(dataLine), &ev) != nil {
			continue
		}
		if sn, ok := ev["sequence_number"].(float64); ok {
			if seen[sn] {
				t.Errorf("duplicate sequence_number %v", sn)
			}
			seen[sn] = true
		}
	}
}

func TestGoogleGenerateContentFunctionCall(t *testing.T) {
	final := "```function_call\n{\"name\": \"get_weather\", \"args\": {\"city\": \"Jakarta\"}}\n```"
	app, _ := newStubbedApp(t, []string{final})
	body := `{
		"contents":[{"role":"user","parts":[{"text":"weather in Jakarta?"}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY"}}
	}`

	req := httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:generateContent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp models.GoogleGenerateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %d", len(resp.Candidates))
	}
	cand := resp.Candidates[0]
	if cand.FinishReason != "STOP" {
		t.Errorf("finishReason = %q, want STOP", cand.FinishReason)
	}
	var found *models.GoogleFunctionCall
	for _, p := range cand.Content.Parts {
		if p.FunctionCall != nil {
			found = p.FunctionCall
		}
		if p.Text != "" && strings.Contains(p.Text, "function_call") {
			t.Errorf("raw function_call syntax leaked into text: %q", p.Text)
		}
	}
	if found == nil {
		t.Fatalf("no functionCall part in response: %+v", cand.Content.Parts)
	}
	if found.Name != "get_weather" {
		t.Errorf("name = %q", found.Name)
	}
	if found.Args["city"] != "Jakarta" {
		t.Errorf("args = %v", found.Args)
	}
}

func TestGoogleStreamGenerateContentFunctionCall(t *testing.T) {
	final := "```function_call\n{\"name\": \"get_weather\", \"args\": {\"city\": \"Jakarta\"}}\n```"
	app, _ := newStubbedApp(t, []string{final})
	body := `{
		"contents":[{"role":"user","parts":[{"text":"weather?"}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}]
	}`

	req := httptest.NewRequest("POST", "/v1beta/models/gemini-3.6-flash:streamGenerateContent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	payloads := parseSSEPayloads(t, rec.Body.String())
	if len(payloads) == 0 {
		t.Fatal("no chunks emitted for a streaming request with tools")
	}

	var sawCall bool
	for _, p := range payloads {
		cands, _ := p["candidates"].([]any)
		for _, c := range cands {
			cm, _ := c.(map[string]any)
			content, _ := cm["content"].(map[string]any)
			parts, _ := content["parts"].([]any)
			for _, pt := range parts {
				pm, _ := pt.(map[string]any)
				if fc, ok := pm["functionCall"].(map[string]any); ok {
					sawCall = true
					if fc["name"] != "get_weather" {
						t.Errorf("functionCall name = %v", fc["name"])
					}
				}
			}
		}
	}
	if !sawCall {
		t.Errorf("streaming response with tools produced no functionCall:\n%s", rec.Body.String())
	}
}

func TestResponsesInputFunctionCallOutputRoundtrip(t *testing.T) {
	// Second turn of a Codex-style loop: the input replays the call and its
	// output. The proxy must label the tool result with the call id.
	app, _ := newStubbedApp(t, []string{"It is 30 degrees in Jakarta."})
	body := fmt.Sprintf(`{
		"model":"gemini-3.6-flash",
		"input":[
			{"role":"user","content":"weather in Jakarta?"},
			{"type":"function_call","call_id":"call_abc123","name":"get_weather","arguments":"{\"city\":\"Jakarta\"}"},
			{"type":"function_call_output","call_id":"call_abc123","output":"{\"temp_c\":30}"}
		],
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "30 degrees") {
		t.Errorf("assistant text missing: %s", out)
	}
}

func TestSlimmingRegressionLargeToolSetStillHasSchema(t *testing.T) {
	// Regression guard for the reported bug: a payload above the old 30KB
	// threshold must still carry parameters, not just name+description.
	app, _ := newStubbedApp(t, []string{"ok"})

	var tools []string
	for i := 0; i < 60; i++ {
		tools = append(tools, fmt.Sprintf(`{"type":"function","function":{
			"name":"tool_%d_%s",
			"description":"%s",
			"parameters":{"type":"object","properties":{
				"path":{"type":"string","description":"%s"},
				"mode":{"type":"string","enum":["r","w"]}
			},"required":["path"],"additionalProperties":false}
		}}`, i, strings.Repeat("n", 40), strings.Repeat("Does a thing. ", 20), strings.Repeat("Path to target. ", 20)))
	}
	body := fmt.Sprintf(`{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"go"}],"tools":[%s]}`, strings.Join(tools, ","))

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	// Rebuild the prompt to inspect it directly.
	var chatReq models.OpenAIChatRequest
	if err := json.Unmarshal([]byte(body), &chatReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, _ := json.Marshal(chatReq.Tools)
	if len(raw) < 30000 {
		t.Fatalf("fixture must exceed the old 30KB slim threshold, got %d", len(raw))
	}
}
