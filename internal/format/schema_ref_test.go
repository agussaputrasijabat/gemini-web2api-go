package format

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// refSchema builds a tool with a $ref-based parameter schema, the shape many
// OpenAI SDKs emit for nested object types.
func refSchema() models.OpenAITool {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"user": map[string]any{"$ref": "#/$defs/User"},
			"tags": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/Tag"}},
		},
		"required": []any{"user"},
		"$defs": map[string]any{
			"User": map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}, "age": map[string]any{"type": "integer"}},
				"required":   []any{"name"},
			},
			"Tag": map[string]any{"type": "string", "enum": []any{"a", "b"}},
		},
	}
	return models.OpenAITool{Type: "function", Function: models.OpenAIFunction{
		Name: "create_user", Description: "Create a user", Parameters: params,
	}}
}

func TestCompressSchemaKeepsDefs(t *testing.T) {
	for level := SchemaDropCosmetic; level <= SchemaPruneDeep; level++ {
		tool := refSchema()
		out := CompressSchema(tool.Function.Parameters, level)
		m, ok := out.(map[string]any)
		if !ok {
			t.Fatalf("level %d: not a map", level)
		}
		defs, hasDefs := m["$defs"]
		if !hasDefs {
			t.Errorf("level %d: $defs dropped while $ref survives", level)
			continue
		}
		dm, _ := defs.(map[string]any)
		if _, ok := dm["User"]; !ok {
			t.Errorf("level %d: $defs.User missing", level)
		}
		raw := mustJSON(t, out)
		if !strings.Contains(raw, "#/$defs/User") || !strings.Contains(raw, "#/$defs/Tag") {
			t.Errorf("level %d: $ref target missing in %s", level, raw)
		}
	}
}

func TestCompressToolDefinitionsRefSchema(t *testing.T) {
	fns := []models.OpenAIFunction{refSchema().Function}
	buf, level := CompressToolDefinitions(fns, MinToolSchemaMaxBytes)
	raw := string(buf)
	if !strings.Contains(raw, "$defs") {
		t.Errorf("level %d: $defs missing from compressed output: %s", level, raw)
	}
	if !strings.Contains(raw, "required") {
		t.Errorf("level %d: required missing: %s", level, raw)
	}
}

func TestCompressToolDefinitionsFloorClampsBudget(t *testing.T) {
	fns := []models.OpenAIFunction{refSchema().Function}

	// Absurd budgets get clamped up to MinToolSchemaMaxBytes: the output must
	// never be so small that the schema is unusable and the model has to invent
	// argument names.
	for _, budget := range []int{1, 100, 500} {
		buf, level := CompressToolDefinitions(fns, budget)
		if len(buf) == 0 {
			t.Fatalf("budget %d: empty output", budget)
		}
		if level == SchemaKeepAll {
			continue // under the floor, nothing to compress is fine
		}
		if !strings.Contains(string(buf), "create_user") {
			t.Errorf("budget %d: tool name missing: %s", budget, string(buf))
		}
		// The structural keys must survive regardless of the budget.
		if !strings.Contains(string(buf), "required") {
			t.Errorf("budget %d: required missing: %s", budget, string(buf))
		}
	}
}

func TestToolResultLabelUsesResolvedName(t *testing.T) {
	req := models.OpenAIChatRequest{
		Tools: []models.OpenAITool{mustReadTool()},
		Messages: []models.OpenAIMessage{
			{Role: "user", Content: "read /etc/hosts"},
			{Role: "assistant", Content: nil, ToolCalls: []models.OpenAIToolCall{{
				ID: "call_123", Type: "function",
				Function: models.OpenAIToolCallFunction{Name: "read", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: "call_123", Content: "data"},
		},
	}
	prompt, _, err := MessagesToPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[Tool result for read (call id call_123)]") {
		t.Errorf("label should carry the tool name, got prompt: %s", prompt)
	}
}

func TestToolResultLabelUnknownID(t *testing.T) {
	req := models.OpenAIChatRequest{
		Tools: []models.OpenAITool{mustReadTool()},
		Messages: []models.OpenAIMessage{
			{Role: "tool", ToolCallID: "call_gone", Content: "data"},
		},
	}
	prompt, _, err := MessagesToPrompt(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "tool name unknown") {
		t.Errorf("unknown id must be labeled explicitly, got: %s", prompt)
	}
	if !strings.Contains(prompt, "call_gone") {
		t.Errorf("id must still be present for pairing, got: %s", prompt)
	}
}

func mustReadTool() models.OpenAITool {
	return models.OpenAITool{Type: "function", Function: models.OpenAIFunction{
		Name: "read", Description: "read a file",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"filePath": map[string]any{"type": "string"},
		}},
	}}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
