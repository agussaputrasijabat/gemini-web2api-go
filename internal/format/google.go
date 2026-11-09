package format

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

type Image struct {
	Data []byte
	MIME string
	URL  string // Remote URL passthrough (when data not yet fetched)
}

func BuildToolPrompt(defs []models.GoogleFunctionDeclaration) string {
	return BuildToolPromptWithOptions(defs, DefaultToolOptions())
}

// BuildToolPromptWithOptions renders Gemini function declarations into the
// prompt under an explicit schema byte budget.
//
// Compression replaces the old name+description-only fallback: the JSON Schema
// (type, properties, required, enum, items, additionalProperties) is always
// kept, because without it the model cannot know argument names.
func BuildToolPromptWithOptions(defs []models.GoogleFunctionDeclaration, opts ToolOptions) string {
	specBytes, level := CompressGoogleFunctionDeclarations(defs, opts.MaxSchemaBytes)
	if level != SchemaKeepAll {
		log.Printf("Google tool defs compressed (level=%s, %d bytes, %d declarations)",
			level, len(specBytes), len(defs))
	}
	return fmt.Sprintf(
		"# Tool Use\n\n"+
			"You can call the following tools to help accomplish tasks. "+
			"These tools connect to the user's local environment and will execute when called.\n"+
			"Each tool's `parameters` is a JSON Schema; argument names and types MUST conform to it.\n\n"+
			"Call format (use this exact format):\n"+
			"```function_call\n"+
			"{\"name\": \"<tool_name>\", \"args\": {<arguments>}}\n"+
			"```\n\n"+
			"When calling tools:\n"+
			"- Output ONLY the function_call block(s), nothing else\n"+
			"- You may call multiple tools with multiple blocks\n"+
			"- After receiving a [Tool result for ...], use that data to answer the user\n\n"+
			"Available tools:\n%s",
		string(specBytes),
	)
}

func GoogleToolChoiceInstruction(req models.GoogleGenerateRequest) string {
	mode := "AUTO"
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil {
		if req.ToolConfig.FunctionCallingConfig.Mode != "" {
			mode = req.ToolConfig.FunctionCallingConfig.Mode
		}
	}

	if mode == "NONE" {
		return "\n\nIMPORTANT: Do NOT call any tools. Respond with text only."
	}
	if mode == "ANY" {
		if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil && len(req.ToolConfig.FunctionCallingConfig.AllowedFunctionNames) > 0 {
			var names []string
			for _, s := range req.ToolConfig.FunctionCallingConfig.AllowedFunctionNames {
				names = append(names, fmt.Sprintf("\"%s\"", s))
			}
			if len(names) > 0 {
				return fmt.Sprintf("\n\nIMPORTANT: You MUST call one of these tools: %s. Do not respond with text only.", strings.Join(names, ", "))
			}
		}
		return "\n\nIMPORTANT: You MUST call at least one tool. Do not respond with text only."
	}
	return ""
}

func GoogleContentsToPrompt(req models.GoogleGenerateRequest) (string, []Image, error) {
	return GoogleContentsToPromptWithOptions(req, DefaultToolOptions())
}

// GoogleContentsToPromptWithOptions converts native Gemini contents into a
// prompt, preserving functionCall/functionResponse roundtrips.
//
// Roundtrip rules, mirroring the native API contract:
//   - a model part with functionCall is rendered as a function_call block so
//     the model sees its own earlier call, including the call id when present;
//   - a user part with functionResponse is rendered as a tool result keyed by
//     the call id (Gemini 3.x requires the id to match the call, otherwise the
//     model can return an empty STOP response);
//   - thoughtSignature is opaque and cannot be replayed through a text prompt,
//     so it is logged at debug level and otherwise ignored.
func GoogleContentsToPromptWithOptions(req models.GoogleGenerateRequest, opts ToolOptions) (string, []Image, error) {
	var parts []string
	var images []Image

	fcMode := "AUTO"
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil && req.ToolConfig.FunctionCallingConfig.Mode != "" {
		fcMode = req.ToolConfig.FunctionCallingConfig.Mode
	}

	var toolDefs []models.GoogleFunctionDeclaration
	if fcMode != "NONE" {
		for _, toolGroup := range req.Tools {
			toolDefs = append(toolDefs, toolGroup.FunctionDeclarations...)
		}
	}

	var sysText string
	if req.SystemInstruction != nil {
		var tParts []string
		for _, p := range req.SystemInstruction.Parts {
			if p.Text != "" {
				tParts = append(tParts, p.Text)
			}
		}
		sysText = strings.Join(tParts, " ")
	}

	if sysText != "" {
		if len(toolDefs) > 0 {
			constraint := GoogleToolChoiceInstruction(req)
			parts = append(parts, sysText+"\n\n"+BuildToolPromptWithOptions(toolDefs, opts)+constraint)
		} else {
			parts = append(parts, sysText)
		}
	} else if len(toolDefs) > 0 {
		constraint := GoogleToolChoiceInstruction(req)
		parts = append(parts, BuildToolPromptWithOptions(toolDefs, opts)+constraint)
	}

	for _, content := range req.Contents {
		role := content.Role
		if role == "" {
			role = "user"
		}

		var msgParts []string
		for _, p := range content.Parts {
			if p.Text != "" {
				msgParts = append(msgParts, p.Text)
			} else if p.InlineData != nil {
				mime := p.InlineData.MIMEType
				if mime == "" {
					mime = "image/png"
				}
				if dec, err := base64.StdEncoding.DecodeString(p.InlineData.Data); err == nil {
					images = append(images, Image{Data: dec, MIME: mime})
					msgParts = append(msgParts, "[Image attached]")
				}
			} else if p.FunctionCall != nil {
				args := p.FunctionCall.Args
				if args == nil {
					args = map[string]any{}
				}
				payload, _ := json.Marshal(map[string]any{"name": p.FunctionCall.Name, "args": args})
				block := fmt.Sprintf("```function_call\n%s\n```", string(payload))
				if p.FunctionCall.ID != "" {
					// Keep the id visible so the following functionResponse can
					// be paired unambiguously in later turns.
					block += fmt.Sprintf("\n[call_id: %s]", p.FunctionCall.ID)
				}
				msgParts = append(msgParts, block)
			} else if p.FunctionResponse != nil {
				resp := p.FunctionResponse.Response
				if resp == nil {
					resp = map[string]any{}
				}
				payload, _ := json.Marshal(resp)
				// Prefer the call id as the label; Gemini 3.x pairs responses to
				// calls by id and rejects mismatches with an empty STOP answer.
				label := p.FunctionResponse.ID
				if label == "" {
					label = p.FunctionResponse.Name
				}
				if label == "" {
					label = "unknown"
				}
				msgParts = append(msgParts, fmt.Sprintf("[Tool result for %s]: %s", label, string(payload)))
			}
		}

		text := strings.Join(msgParts, "\n")
		if role == "model" {
			parts = append(parts, fmt.Sprintf("[Assistant]: %s", text))
		} else {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n\n"), images, nil
}

// ParseGoogleFunctionCalls extracts native-Gemini function calls from model
// output. It shares the tolerant extraction logic with ParseToolCalls but maps
// to GoogleFunctionCall (map args, optional id).
//
// Returning an empty call list with the original text is the safe failure mode:
// the caller should emit plain text rather than a fabricated call.
func ParseGoogleFunctionCalls(text string) (string, []models.GoogleFunctionCall) {
	clean, payloads := extractPayloads(text)
	if len(payloads) == 0 {
		trimmed := strings.TrimSpace(text)
		if len(trimmed) > 2 && trimmed[0] == '{' {
			if name, _, args, id, ok := parseCallPayload(trimmed); ok {
				return "", []models.GoogleFunctionCall{{Name: name, Args: args, ID: id}}
			}
		}
		return text, nil
	}

	var calls []models.GoogleFunctionCall
	for _, payload := range payloads {
		name, _, args, id, ok := parseCallPayload(payload)
		if !ok {
			continue
		}
		calls = append(calls, models.GoogleFunctionCall{Name: name, Args: args, ID: id})
	}

	return clean, calls
}
