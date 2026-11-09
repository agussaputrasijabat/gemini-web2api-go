package format

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// TestResponsesFunctionCallItemsRoundtrip covers the Codex CLI contract: a flat
// input list where function_call and function_call_output are siblings.
func TestResponsesFunctionCallItemsRoundtrip(t *testing.T) {
	input := []any{
		map[string]any{"role": "user", "content": "What is the weather in Jakarta?"},
		map[string]any{
			"type":      "function_call",
			"call_id":   "call_abc",
			"name":      "get_weather",
			"arguments": `{"city":"Jakarta"}`,
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_abc",
			"output":  `{"temp_c":30}`,
		},
	}

	messages, err := ResponsesInputToMessages(input, "")
	if err != nil {
		t.Fatalf("ResponsesInputToMessages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %v", len(messages), messages)
	}

	// user
	if messages[0]["role"] != "user" {
		t.Errorf("messages[0].role = %v", messages[0]["role"])
	}

	// assistant with the tool call
	assistant := messages[1]
	if assistant["role"] != "assistant" {
		t.Errorf("messages[1].role = %v", assistant["role"])
	}
	calls, ok := assistant["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant.tool_calls = %v", assistant["tool_calls"])
	}
	if calls[0]["id"] != "call_abc" {
		t.Errorf("call id = %v", calls[0]["id"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("call name = %v", fn["name"])
	}
	if fn["arguments"] != `{"city":"Jakarta"}` {
		t.Errorf("call arguments = %v", fn["arguments"])
	}

	// tool result keyed by call id
	toolMsg := messages[2]
	if toolMsg["role"] != "tool" {
		t.Errorf("messages[2].role = %v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "call_abc" {
		t.Errorf("tool_call_id = %v", toolMsg["tool_call_id"])
	}
	if toolMsg["content"] != `{"temp_c":30}` {
		t.Errorf("tool output = %v", toolMsg["content"])
	}
}

func TestResponsesConsecutiveFunctionCallsMergeIntoOneAssistantTurn(t *testing.T) {
	// Parallel calls arrive as sibling items but belong to a single assistant
	// message, which is what OpenAI clients and this proxy expect.
	input := []any{
		map[string]any{"role": "user", "content": "both please"},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "a", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": "call_2", "name": "b", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "r1"},
		map[string]any{"type": "function_call_output", "call_id": "call_2", "output": "r2"},
	}

	messages, err := ResponsesInputToMessages(input, "")
	if err != nil {
		t.Fatalf("ResponsesInputToMessages: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages (user + assistant + 2 tool), got %d: %v", len(messages), messages)
	}
	calls, ok := messages[1]["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 2 {
		t.Fatalf("expected 2 merged tool calls, got %v", messages[1]["tool_calls"])
	}
	if messages[2]["tool_call_id"] != "call_1" || messages[3]["tool_call_id"] != "call_2" {
		t.Errorf("tool ids = %v, %v", messages[2]["tool_call_id"], messages[3]["tool_call_id"])
	}
}

func TestResponsesFunctionCallOutputStructuredOutput(t *testing.T) {
	// Some clients send output as JSON, not a string.
	input := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": map[string]any{"ok": true}},
	}
	messages, err := ResponsesInputToMessages(input, "")
	if err != nil {
		t.Fatalf("ResponsesInputToMessages: %v", err)
	}
	toolMsg := messages[len(messages)-1]
	out, _ := toolMsg["content"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("structured output not marshaled: %q", out)
	}
	if parsed["ok"] != true {
		t.Errorf("parsed = %v", parsed)
	}
}

func TestResponsesInputGeneratesCallIDWhenMissing(t *testing.T) {
	input := []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"type": "function_call", "name": "f", "arguments": "{}"},
	}
	messages, err := ResponsesInputToMessages(input, "")
	if err != nil {
		t.Fatalf("ResponsesInputToMessages: %v", err)
	}
	calls := messages[1]["tool_calls"].([]map[string]any)
	if calls[0]["id"] == "" || calls[0]["id"] == nil {
		t.Error("expected a generated call id")
	}
}

func TestResponsesFunctionCallInsideAssistantContent(t *testing.T) {
	// The nested (message.content) shape must keep working.
	input := []any{
		map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "checking"},
				map[string]any{"type": "function_call", "call_id": "c9", "name": "f", "arguments": `{"a":1}`},
			},
		},
		map[string]any{"type": "function_call_output", "call_id": "c9", "output": "done"},
	}
	messages, err := ResponsesInputToMessages(input, "")
	if err != nil {
		t.Fatalf("ResponsesInputToMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0]["content"] != "checking" {
		t.Errorf("assistant text = %v", messages[0]["content"])
	}
	calls := messages[0]["tool_calls"].([]map[string]any)
	if calls[0]["id"] != "c9" {
		t.Errorf("call id = %v", calls[0]["id"])
	}
	if messages[1]["tool_call_id"] != "c9" {
		t.Errorf("tool_call_id = %v", messages[1]["tool_call_id"])
	}
}

func TestResponsesFullRoundtripThroughMessagesToPrompt(t *testing.T) {
	// End-to-end: Responses input -> messages -> prompt must keep the call id
	// so the model can pair the result with its own earlier call.
	input := []any{
		map[string]any{"role": "user", "content": "weather?"},
		map[string]any{"type": "function_call", "call_id": "call_777", "name": "get_weather", "arguments": `{"city":"Jakarta"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_777", "output": `{"temp":30}`},
	}
	messages, err := ResponsesInputToMessages(input, "Be terse.")
	if err != nil {
		t.Fatalf("ResponsesInputToMessages: %v", err)
	}

	var reqMsgs []models.OpenAIMessage
	for _, m := range messages {
		msg := models.OpenAIMessage{Role: m["role"].(string), Content: m["content"]}
		if id, ok := m["tool_call_id"].(string); ok {
			msg.ToolCallID = id
		}
		if name, ok := m["name"].(string); ok {
			msg.Name = name
		}
		if rc, ok := m["tool_calls"].([]map[string]any); ok {
			for _, r := range rc {
				tc := models.OpenAIToolCall{}
				tc.ID, _ = r["id"].(string)
				tc.Type, _ = r["type"].(string)
				if fn, ok := r["function"].(map[string]any); ok {
					tc.Function.Name, _ = fn["name"].(string)
					tc.Function.Arguments, _ = fn["arguments"].(string)
				}
				msg.ToolCalls = append(msg.ToolCalls, tc)
			}
		}
		reqMsgs = append(reqMsgs, msg)
	}

	prompt, _, err := MessagesToPrompt(models.OpenAIChatRequest{Messages: reqMsgs, ToolChoice: "auto"})
	if err != nil {
		t.Fatalf("MessagesToPrompt: %v", err)
	}
	// The system instruction must survive.
	if !strings.Contains(prompt, "Be terse.") {
		t.Errorf("instructions lost: %q", prompt)
	}
	// The assistant call and the tool result must both reference the id.
	if !strings.Contains(prompt, "call_777") {
		t.Errorf("call id lost between turns:\n%s", prompt)
	}
}

func TestBuildResponseOutputUsesDistinctItemAndCallIDs(t *testing.T) {
	out := BuildResponseOutput("", []models.OpenAIToolCall{{
		ID:       "call_x",
		Type:     "function",
		Function: models.OpenAIToolCallFunction{Name: "f", Arguments: `{"a":1}`},
	}}, "msg_1")

	if len(out) != 1 {
		t.Fatalf("expected 1 item, got %d", len(out))
	}
	item := out[0]
	if item["type"] != "function_call" {
		t.Errorf("type = %v", item["type"])
	}
	if item["call_id"] != "call_x" {
		t.Errorf("call_id = %v", item["call_id"])
	}
	// The item id is a separate identifier in the Responses schema.
	if item["id"] == item["call_id"] {
		t.Errorf("id and call_id must differ, both = %v", item["id"])
	}
	if item["status"] != "completed" {
		t.Errorf("status = %v", item["status"])
	}
	if item["arguments"] != `{"a":1}` {
		t.Errorf("arguments = %v", item["arguments"])
	}
}

func TestBuildResponseOutputTextOnly(t *testing.T) {
	out := BuildResponseOutput("hi", nil, "msg_2")
	if len(out) != 1 {
		t.Fatalf("expected 1 item, got %d", len(out))
	}
	if out[0]["type"] != "message" {
		t.Errorf("type = %v", out[0]["type"])
	}
	if out[0]["id"] != "msg_2" {
		t.Errorf("id = %v", out[0]["id"])
	}
}
