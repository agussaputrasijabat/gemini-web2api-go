package format

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

func TestParseToolCallsMalformedJSON(t *testing.T) {
	// Trailing comma: invalid JSON. The block is still removed from the text so
	// the user never sees raw tool syntax, and no bogus call is emitted.
	text := "Trying.\n```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\",}}\n```"
	clean, calls := ParseToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls for malformed JSON, got %d", len(calls))
	}
	if strings.Contains(clean, "```") {
		t.Errorf("tool block should be stripped, got %q", clean)
	}
}

func TestParseToolCallsMissingClosingFence(t *testing.T) {
	text := "```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}"
	clean, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("name = %q", calls[0].Function.Name)
	}
	if strings.TrimSpace(clean) != "" {
		t.Errorf("clean = %q, want empty", clean)
	}
}

func TestParseToolCallsBackticksInsideArguments(t *testing.T) {
	text := "```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"content\": \"```go\\nfunc main() {}\\n```\"}}\n```"
	clean, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d (clean=%q)", len(calls), clean)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if !strings.Contains(args["content"].(string), "func main()") {
		t.Errorf("backticked content was truncated: %v", args["content"])
	}
	if strings.TrimSpace(clean) != "" {
		t.Errorf("clean = %q, want empty", clean)
	}
}

func TestParseToolCallsBracesInsideStringArguments(t *testing.T) {
	// A naive brace counter would stop at the "}" inside the string.
	text := "```tool_call\n{\"name\": \"render\", \"arguments\": {\"template\": \"{{name}} says }\"}}\n```"
	_, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("bad arguments JSON: %v", err)
	}
	if args["template"] != "{{name}} says }" {
		t.Errorf("template = %v", args["template"])
	}
}

func TestParseToolCallsMultipleBlocks(t *testing.T) {
	text := "Doing two things.\n" +
		"```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n```\n" +
		"```tool_call\n{\"name\": \"get_time\", \"arguments\": {\"zone\": \"Asia/Jakarta\"}}\n```\n"
	clean, calls := ParseToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" || calls[1].Function.Name != "get_time" {
		t.Errorf("names = %q, %q", calls[0].Function.Name, calls[1].Function.Name)
	}
	// Ids must be unique across parallel calls.
	if calls[0].ID == calls[1].ID {
		t.Errorf("duplicate ids: %q", calls[0].ID)
	}
	if !strings.Contains(clean, "Doing two things.") {
		t.Errorf("prose lost: %q", clean)
	}
}

func TestParseToolCallsParallelSameName(t *testing.T) {
	text := "```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a\"}}\n```\n" +
		"```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"b\"}}\n```"
	_, calls := ParseToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID == calls[1].ID {
		t.Error("parallel calls to the same tool must have distinct ids")
	}
	if calls[0].Function.Arguments == calls[1].Function.Arguments {
		t.Error("arguments should differ")
	}
}

func TestParseToolCallsProseMixedWithBlocks(t *testing.T) {
	text := "Let me check the weather.\n\n```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Jakarta\"}}\n```\n\nThat should do it."
	clean, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if !strings.Contains(clean, "Let me check the weather.") {
		t.Errorf("leading prose lost: %q", clean)
	}
	if !strings.Contains(clean, "That should do it.") {
		t.Errorf("trailing prose lost: %q", clean)
	}
}

func TestParseToolCallsNoBlocks(t *testing.T) {
	text := "Just a normal answer with no tools."
	clean, calls := ParseToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls, got %d", len(calls))
	}
	if clean != text {
		t.Errorf("clean = %q, want unchanged", clean)
	}
}

func TestParseToolCallsBareJSON(t *testing.T) {
	text := `{"name": "get_weather", "arguments": {"city": "Jakarta"}}`
	clean, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call for bare JSON, got %d", len(calls))
	}
	if strings.TrimSpace(clean) != "" {
		t.Errorf("clean = %q, want empty", clean)
	}
}

func TestParseToolCallsOrdinaryJSONIsNotACall(t *testing.T) {
	// A plain JSON object without an arguments key must not be mistaken for a
	// tool call: that would silently swallow a legitimate answer.
	text := `{"name": "Agus", "city": "Jakarta"}`
	_, calls := ParseToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("plain JSON was treated as a tool call: %+v", calls)
	}
}

func TestParseToolCallsArgumentsAsString(t *testing.T) {
	text := "```tool_call\n{\"name\": \"get_weather\", \"arguments\": \"{\\\"city\\\":\\\"Jakarta\\\"}\"}\n```"
	_, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not normalized: %v (%q)", err, calls[0].Function.Arguments)
	}
	if args["city"] != "Jakarta" {
		t.Errorf("city = %v", args["city"])
	}
}

func TestParseToolCallsFunctionCallTag(t *testing.T) {
	text := "```function_call\n{\"name\": \"get_weather\", \"args\": {\"city\": \"Bandung\"}}\n```"
	_, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("name = %q", calls[0].Function.Name)
	}
}

func TestParseToolCallsPreservesModelCallID(t *testing.T) {
	text := "```tool_call\n{\"name\": \"get_weather\", \"id\": \"call_abc\", \"arguments\": {}}\n```"
	_, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].ID != "call_abc" {
		t.Errorf("id = %q, want call_abc", calls[0].ID)
	}
}

func TestParseToolCallsBareTagPrefix(t *testing.T) {
	text := "tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Surabaya\"}}\n"
	_, calls := ParseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
}

func TestParseGoogleFunctionCallsMultiple(t *testing.T) {
	text := "```function_call\n{\"name\": \"a\", \"args\": {\"x\": 1}}\n```\n" +
		"```function_call\n{\"name\": \"b\", \"args\": {\"y\": 2}}\n```"
	clean, calls := ParseGoogleFunctionCalls(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "a" || calls[1].Name != "b" {
		t.Errorf("names = %q, %q", calls[0].Name, calls[1].Name)
	}
	if len(calls[0].Args) != 1 {
		t.Errorf("args lost: %v", calls[0].Args)
	}
	if strings.TrimSpace(clean) != "" {
		t.Errorf("clean = %q, want empty", clean)
	}
}

func TestParseGoogleFunctionCallsMalformed(t *testing.T) {
	text := "```function_call\n{not json}\n```"
	_, calls := ParseGoogleFunctionCalls(text)
	if len(calls) != 0 {
		t.Fatalf("expected 0 calls, got %d", len(calls))
	}
}

func TestParseGoogleFunctionCallsKeepsID(t *testing.T) {
	text := "```function_call\n{\"name\": \"lookup\", \"id\": \"call_9\", \"args\": {}}\n```"
	_, calls := ParseGoogleFunctionCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].ID != "call_9" {
		t.Errorf("id = %q", calls[0].ID)
	}
}

func TestMessagesToPromptUsesToolCallID(t *testing.T) {
	req := models.OpenAIChatRequest{
		Messages: []models.OpenAIMessage{
			{Role: "user", Content: "weather?"},
			{
				Role: "assistant",
				ToolCalls: []models.OpenAIToolCall{{
					ID:       "call_xyz",
					Type:     "function",
					Function: models.OpenAIToolCallFunction{Name: "get_weather", Arguments: `{"city":"Jakarta"}`},
				}},
			},
			{Role: "tool", ToolCallID: "call_xyz", Name: "get_weather", Content: `{"temp":30}`},
		},
		ToolChoice: "auto",
	}

	prompt, _, err := MessagesToPrompt(req)
	if err != nil {
		t.Fatalf("MessagesToPrompt: %v", err)
	}
	// The result must be labelled with the tool name (so the model knows what
	// produced the data) plus the call id (so parallel calls stay distinguishable).
	if !strings.Contains(prompt, "[Tool result for get_weather (call id call_xyz)]") {
		t.Errorf("prompt missing id-labelled tool result:\n%s", prompt)
	}
	// The assistant turn must echo the id so the model can pair them.
	if !strings.Contains(prompt, "call_xyz") {
		t.Errorf("assistant tool call block missing id:\n%s", prompt)
	}
}

func TestMessagesToPromptFallsBackToNameWithoutID(t *testing.T) {
	req := models.OpenAIChatRequest{
		Messages: []models.OpenAIMessage{
			{Role: "user", Content: "weather?"},
			{Role: "tool", Name: "get_weather", Content: `{"temp":30}`},
		},
		ToolChoice: "auto",
	}
	prompt, _, err := MessagesToPrompt(req)
	if err != nil {
		t.Fatalf("MessagesToPrompt: %v", err)
	}
	if !strings.Contains(prompt, "[Tool result for get_weather]") {
		t.Errorf("prompt missing name-based tool result:\n%s", prompt)
	}
}

func TestToolChoiceRequiredInstruction(t *testing.T) {
	instr := BuildToolChoiceInstruction("required")
	if !strings.Contains(instr, "MUST call at least one tool") {
		t.Errorf("required instruction = %q", instr)
	}
	if BuildToolChoiceMode := ToolChoiceMode("required"); BuildToolChoiceMode != "required" {
		t.Errorf("mode = %q", BuildToolChoiceMode)
	}
}

func TestToolChoiceNamedObject(t *testing.T) {
	tc := map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
	instr := BuildToolChoiceInstruction(tc)
	if !strings.Contains(instr, "get_weather") {
		t.Errorf("named instruction = %q", instr)
	}
	if ToolChoiceName(tc) != "get_weather" {
		t.Errorf("ToolChoiceName = %q", ToolChoiceName(tc))
	}
	if ToolChoiceMode(tc) != "named" {
		t.Errorf("mode = %q", ToolChoiceMode(tc))
	}
}

func TestToolChoiceModes(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"auto", "auto"},
		{"none", "none"},
		{"NONE", "none"},
		{"ANY", "required"},
		{nil, "auto"},
		{map[string]any{"name": "x"}, "named"},
	}
	for _, c := range cases {
		if got := ToolChoiceMode(c.in); got != c.want {
			t.Errorf("ToolChoiceMode(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScanJSONValueBalanced(t *testing.T) {
	text := `{"a": {"b": [1, 2, {"c": "}"}]}, "d": "{"}`
	got, end, ok := scanJSONValue(text, 0)
	if !ok {
		t.Fatal("scanJSONValue failed")
	}
	if end != len(text) {
		t.Errorf("end = %d, want %d", end, len(text))
	}
	if got != text {
		t.Errorf("got = %q", got)
	}
}
