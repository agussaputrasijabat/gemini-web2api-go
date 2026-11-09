package format

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// bigToolSchema builds a definition set comfortably above any sane budget while
// keeping a meaningful nested shape: object -> object -> array of objects.
func bigToolSchema() []models.OpenAIFunction {
	var fns []models.OpenAIFunction
	for i := 0; i < 40; i++ {
		fns = append(fns, models.OpenAIFunction{
			Name:        "tool_" + strings.Repeat("n", 8) + string(rune('a'+i%26)),
			Description: strings.Repeat("Use this tool to do a very important thing. ", 12),
			Parameters: map[string]any{
				"$schema": "http://json-schema.org/draft-07/schema#",
				"type":    "object",
				"title":   "ToolArguments",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": strings.Repeat("Absolute path to the file. ", 10),
						"examples":    []any{"/tmp/a.txt", "/tmp/b.txt"},
						"default":     "/tmp/a.txt",
					},
					"mode": map[string]any{
						"type": "string",
						"enum": []any{"read", "write", "append"},
					},
					"options": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"recursive": map[string]any{"type": "boolean", "default": false},
							"depth":     map[string]any{"type": "integer", "description": strings.Repeat("How deep. ", 20)},
							"filters": map[string]any{
								"type": "array",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"glob":   map[string]any{"type": "string", "description": strings.Repeat("Glob pattern. ", 15)},
										"negate": map[string]any{"type": "boolean"},
									},
									"required": []any{"glob"},
								},
							},
						},
					},
				},
				"required":             []any{"path"},
				"additionalProperties": false,
			},
		})
	}
	return fns
}

func TestCompressToolDefinitionsKeepsSchemaShape(t *testing.T) {
	fns := bigToolSchema()

	raw, _ := json.Marshal(fns)
	if len(raw) <= DefaultToolSchemaMaxBytes {
		t.Fatalf("fixture must exceed the budget, got %d bytes", len(raw))
	}

	out, level := CompressToolDefinitions(fns, DefaultToolSchemaMaxBytes)
	if level == SchemaKeepAll {
		t.Fatal("expected compression for an oversized definition set")
	}
	if len(out) >= len(raw) {
		t.Fatalf("compression did not reduce size: %d -> %d", len(raw), len(out))
	}

	var decoded []map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("compressed output is not valid JSON: %v", err)
	}
	if len(decoded) != len(fns) {
		t.Fatalf("tool count changed: %d -> %d", len(fns), len(decoded))
	}

	for i, tool := range decoded {
		name, _ := tool["name"].(string)
		if name != fns[i].Name {
			t.Errorf("tool %d name = %q, want %q", i, name, fns[i].Name)
		}
		// The schema must survive. This is the regression guard for the
		// name+description-only "slimming" bug.
		params, ok := tool["parameters"].(map[string]any)
		if !ok {
			t.Fatalf("tool %d lost its parameters object", i)
		}
		if params["type"] != "object" {
			t.Errorf("tool %d lost parameters.type: %v", i, params["type"])
		}
		props, ok := params["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Fatalf("tool %d lost its property names", i)
		}
		for _, want := range []string{"path", "mode", "options"} {
			if _, ok := props[want]; !ok {
				t.Errorf("tool %d lost property %q", i, want)
			}
		}
		req, ok := params["required"].([]any)
		if !ok || len(req) != 1 || req[0] != "path" {
			t.Errorf("tool %d lost required: %v", i, params["required"])
		}
		if ap, ok := params["additionalProperties"]; !ok || ap != false {
			t.Errorf("tool %d lost additionalProperties: %v", i, params["additionalProperties"])
		}
		mode, _ := props["mode"].(map[string]any)
		if enum, ok := mode["enum"].([]any); !ok || len(enum) != 3 {
			t.Errorf("tool %d lost enum: %v", i, mode["enum"])
		}
		if _, ok := props["path"].(map[string]any)["type"]; !ok {
			t.Errorf("tool %d lost path.type", i)
		}
	}
}

func TestCompressToolDefinitionsDropsCosmeticKeys(t *testing.T) {
	fns := bigToolSchema()

	out, level := CompressToolDefinitions(fns, DefaultToolSchemaMaxBytes)
	if level == SchemaKeepAll {
		t.Fatal("expected compression at the default budget")
	}

	var decoded []map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	params := decoded[0]["parameters"].(map[string]any)
	for _, cosmetic := range []string{"$schema", "title"} {
		if _, ok := params[cosmetic]; ok {
			t.Errorf("cosmetic key %q should be gone", cosmetic)
		}
	}
	if params["type"] != "object" {
		t.Errorf("type must survive, got %v", params["type"])
	}
	if _, ok := params["required"]; !ok {
		t.Error("required must survive")
	}
}

func TestCompressToolDefinitionsDoesNotMutateInput(t *testing.T) {
	fns := bigToolSchema()
	before, _ := json.Marshal(fns)
	_, _ = CompressToolDefinitions(fns, 500)
	after, _ := json.Marshal(fns)
	if string(before) != string(after) {
		t.Error("CompressToolDefinitions mutated its input")
	}
}

func TestCompressToolDefinitionsUnderBudgetIsUnchanged(t *testing.T) {
	fns := []models.OpenAIFunction{{
		Name:        "ping",
		Description: "Ping.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}}
	out, level := CompressToolDefinitions(fns, DefaultToolSchemaMaxBytes)
	if level != SchemaKeepAll {
		t.Errorf("level = %v, want SchemaKeepAll", level)
	}
	raw, _ := json.Marshal(fns)
	if string(out) != string(raw) {
		t.Error("output should be byte-identical when under budget")
	}
}

func TestCompressGoogleFunctionDeclarationsKeepsSchema(t *testing.T) {
	var defs []models.GoogleFunctionDeclaration
	for i := 0; i < 30; i++ {
		defs = append(defs, models.GoogleFunctionDeclaration{
			Name:        "g_tool_" + strings.Repeat("x", 6) + string(rune('a'+i%26)),
			Description: strings.Repeat("Does something useful. ", 15),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": strings.Repeat("Search query. ", 20),
					},
					"limit": map[string]any{"type": "integer"},
				},
				"required": []any{"query"},
			},
		})
	}

	raw, _ := json.Marshal(defs)
	if len(raw) <= 1000 {
		t.Fatalf("fixture too small: %d", len(raw))
	}

	out, level := CompressGoogleFunctionDeclarations(defs, 1000)
	if level == SchemaKeepAll {
		t.Fatal("expected compression")
	}
	if len(out) >= len(raw) {
		t.Errorf("no size reduction: %d -> %d", len(raw), len(out))
	}

	var decoded []map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	params, ok := decoded[0]["parameters"].(map[string]any)
	if !ok {
		t.Fatal("declaration lost parameters")
	}
	if params["type"] != "object" {
		t.Error("parameters.type lost")
	}
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Error("property names lost")
	}
	if _, ok := params["required"]; !ok {
		t.Error("required lost")
	}
}

func TestToolSchemaThresholdIsConfigurable(t *testing.T) {
	fns := bigToolSchema()
	raw, _ := json.Marshal(fns)

	// A generous budget must not compress.
	_, level := CompressToolDefinitions(fns, len(raw)+1)
	if level != SchemaKeepAll {
		t.Errorf("level = %v with a generous budget, want SchemaKeepAll", level)
	}

	// A tight budget must compress.
	_, level = CompressToolDefinitions(fns, 2000)
	if level == SchemaKeepAll {
		t.Error("expected compression with a tight budget")
	}
}

func TestMessagesToPromptInjectsFullSchema(t *testing.T) {
	fns := bigToolSchema()
	tools := make([]models.OpenAITool, len(fns))
	for i, fn := range fns {
		tools[i] = models.OpenAITool{Type: "function", Function: fn}
	}

	req := models.OpenAIChatRequest{
		Messages:   []models.OpenAIMessage{{Role: "user", Content: "go"}},
		Tools:      tools,
		ToolChoice: "auto",
	}
	prompt, _, err := MessagesToPrompt(req)
	if err != nil {
		t.Fatalf("MessagesToPrompt: %v", err)
	}

	// After compression the payload must still describe the argument contract.
	for _, needle := range []string{`"parameters"`, `"required"`, `"properties"`, `"enum"`, `"additionalProperties"`} {
		if !strings.Contains(prompt, needle) {
			t.Errorf("prompt lost %s; tool schema was discarded", needle)
		}
	}
	// Cosmetic noise should be gone.
	if strings.Contains(prompt, `"$schema"`) {
		t.Error("prompt still contains $schema after compression")
	}
}
