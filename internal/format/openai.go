package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
	"github.com/ikhsan3adi/gemini-web2api/internal/multimodal"
)

var reDataURL = regexp.MustCompile(`(?s)^data:([^;,]+)?(;base64)?,(.*)$`)

func RandHex(n int) string {
	bytes := make([]byte, (n+1)/2)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)[:n]
}

// DecodeDataURL decodes a `data:<mime>[;base64],<payload>` URL into bytes and MIME.
// Mirrors upstream _decode_data_url: base64 payloads are strictly decoded,
// non-base64 payloads are percent-decoded.
func DecodeDataURL(dataURL string) ([]byte, string, error) {
	m := reDataURL.FindStringSubmatch(dataURL)
	if m == nil {
		return nil, "", fmt.Errorf("not a data URL")
	}
	mime := m[1]
	if mime == "" {
		mime = "image/png"
	}
	payload := m[3]
	if m[2] != "" {
		decoded, err := multimodal.DecodeBase64Raw(payload)
		if err != nil {
			return nil, "", fmt.Errorf("base64 decode failed: %w", err)
		}
		if m[1] == "" {
			mime = multimodal.DetectImageMime(decoded)
		}
		return decoded, mime, nil
	}
	decodedStr, err := url.PathUnescape(payload)
	if err != nil {
		return nil, "", fmt.Errorf("data URL percent-decode failed: %w", err)
	}
	decoded := []byte(decodedStr)
	if m[1] == "" {
		mime = multimodal.DetectImageMime(decoded)
	}
	return decoded, mime, nil
}

// ImageFromURL returns an Image with URL set for remote passthrough.
func ImageFromURL(rawURL string) Image {
	// Try to extract MIME from URL extension
	mime := "image/png"
	if extIdx := strings.LastIndex(rawURL, "."); extIdx != -1 {
		ext := strings.ToLower(rawURL[extIdx:])
		// Strip query params
		if qIdx := strings.Index(ext, "?"); qIdx != -1 {
			ext = ext[:qIdx]
		}
		switch ext {
		case ".jpg", ".jpeg":
			mime = "image/jpeg"
		case ".png":
			mime = "image/png"
		case ".gif":
			mime = "image/gif"
		case ".webp":
			mime = "image/webp"
		case ".bmp":
			mime = "image/bmp"
		case ".svg":
			mime = "image/svg+xml"
		}
	}
	return Image{URL: rawURL, MIME: mime}
}

// imageFromURLString resolves a URL string (remote or data:) into an Image.
func imageFromURLString(rawURL, mimeHint string) (Image, bool) {
	if rawURL == "" {
		return Image{}, false
	}
	if strings.HasPrefix(rawURL, "data:") {
		data, mime, err := DecodeDataURL(rawURL)
		if err != nil {
			return Image{}, false
		}
		return Image{Data: data, MIME: mime}, true
	}
	img := ImageFromURL(rawURL)
	if mimeHint != "" {
		img.MIME = mimeHint
	}
	return img, true
}

func partMime(m map[string]any) string {
	for _, k := range []string{"mime_type", "mime", "media_type"} {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	return ""
}

// ImageFromPart parses an OpenAI-style content part (image_url, input_image, image) into an Image.
// Mirrors upstream _image_from_part, including image_url as a plain string.
func ImageFromPart(mapItem map[string]any) (Image, bool) {
	iType, _ := mapItem["type"].(string)

	switch iType {
	case "image_url":
		raw := mapItem["image_url"]
		if urlObj, ok := raw.(map[string]any); ok {
			urlStr, _ := urlObj["url"].(string)
			mimeHint := partMime(urlObj)
			return imageFromURLString(urlStr, mimeHint)
		}
		if urlStr, ok := raw.(string); ok {
			return imageFromURLString(urlStr, "")
		}
	case "input_image", "image":
		var urlRaw any
		if u, ok := mapItem["image_url"]; ok {
			urlRaw = u
		} else if u, ok := mapItem["url"]; ok {
			urlRaw = u
		}
		if urlObj, ok := urlRaw.(map[string]any); ok {
			urlStr, _ := urlObj["url"].(string)
			mimeHint := partMime(urlObj)
			if mimeHint == "" {
				mimeHint = partMime(mapItem)
			}
			if urlStr != "" {
				return imageFromURLString(urlStr, mimeHint)
			}
		} else if urlStr, ok := urlRaw.(string); ok && urlStr != "" {
			if mimeHint := partMime(mapItem); mimeHint != "" {
				return imageFromURLString(urlStr, mimeHint)
			}
			return imageFromURLString(urlStr, "")
		}
		var imageData string
		if s, ok := mapItem["data"].(string); ok && s != "" {
			imageData = s
		} else if s, ok := mapItem["base64"].(string); ok && s != "" {
			imageData = s
		}
		if imageData != "" {
			mime := partMime(mapItem)
			if strings.HasPrefix(imageData, "data:") {
				data, decMime, err := DecodeDataURL(imageData)
				if err != nil {
					return Image{}, false
				}
				if mime == "" {
					mime = decMime
				}
				return Image{Data: data, MIME: mime}, true
			}
			decoded, err := multimodal.DecodeBase64Raw(imageData)
			if err != nil {
				return Image{}, false
			}
			if mime == "" {
				mime = multimodal.DetectImageMime(decoded)
			}
			return Image{Data: decoded, MIME: mime}, true
		}
	}

	return Image{}, false
}

// BuildToolChoiceInstruction returns a prompt directive for tool_choice.
//
// It is advisory only: this proxy drives Gemini's web UI through a prompt, so
// there is no server-side constrained decoding to force a call. "required" and
// a named tool therefore become a strong instruction, not a guarantee. Callers
// must not treat a missing tool call as a protocol violation.
func BuildToolChoiceInstruction(toolChoice any) string {
	if strChoice, ok := toolChoice.(string); ok {
		switch strChoice {
		case "none":
			return "\n\nIMPORTANT: Do NOT call any tools. Respond with text only."
		case "required", "any":
			return "\n\nIMPORTANT: You MUST call at least one tool. Output only tool_call block(s), " +
				"no prose. Do not respond with text only."
		}
	} else if mapChoice, ok := toolChoice.(map[string]any); ok {
		// OpenAI: {"type":"function","function":{"name":"x"}}
		// Legacy OpenAI: {"name":"x"}
		// Gemini:      {"functionCallingConfig":{...}} is handled separately.
		if fn, ok := mapChoice["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				return fmt.Sprintf("\n\nIMPORTANT: You MUST call exactly the tool %q and no other tool. "+
					"Output only the tool_call block, no prose.", name)
			}
		}
		if name, ok := mapChoice["name"].(string); ok && name != "" {
			return fmt.Sprintf("\n\nIMPORTANT: You MUST call exactly the tool %q and no other tool. "+
				"Output only the tool_call block, no prose.", name)
		}
	}
	return ""
}

// ToolChoiceName returns the specific function name requested by a
// tool_choice object, or "" when none was named.
func ToolChoiceName(toolChoice any) string {
	m, ok := toolChoice.(map[string]any)
	if !ok {
		return ""
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	if name, ok := m["name"].(string); ok {
		return name
	}
	return ""
}

// ToolChoiceMode classifies a tool_choice value into none/required/named/auto.
// Gemini's uppercase modes (NONE/ANY/AUTO) are normalized too, so a single
// helper serves all three endpoints.
func ToolChoiceMode(toolChoice any) string {
	name := ToolChoiceName(toolChoice)
	if name != "" {
		return "named"
	}
	s, ok := toolChoice.(string)
	if !ok {
		return "auto"
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return "none"
	case "required", "any":
		return "required"
	}
	return "auto"
}

// ToolOptions controls how tool definitions are rendered into the prompt.
type ToolOptions struct {
	// MaxSchemaBytes is the byte budget for the serialized tool definition
	// block. Zero means DefaultToolSchemaMaxBytes.
	MaxSchemaBytes int
}

// DefaultToolOptions returns the options used when the caller has no config.
func DefaultToolOptions() ToolOptions {
	return ToolOptions{MaxSchemaBytes: DefaultToolSchemaMaxBytes}
}

// MessagesToPrompt renders a chat request into the prompt text Gemini web
// consumes, extracting images for separate upload.
func MessagesToPrompt(req models.OpenAIChatRequest) (string, []Image, error) {
	return MessagesToPromptWithOptions(req, DefaultToolOptions())
}

// MessagesToPromptWithOptions is MessagesToPrompt with an explicit budget for
// the injected tool definitions.
func MessagesToPromptWithOptions(req models.OpenAIChatRequest, opts ToolOptions) (string, []Image, error) {
	var parts []string
	var images []Image

	// callNames maps a tool_call id to the function name it invoked. OpenAI
	// clients only echo the id back on role:"tool" messages, so the name must
	// come from the assistant turn that issued the call.
	callNames := make(map[string]string)
	for _, msg := range req.Messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				callNames[tc.ID] = tc.Function.Name
			}
		}
	}

	strChoice, isStr := req.ToolChoice.(string)
	if (!isStr || strChoice != "none") && len(req.Tools) > 0 {
		var toolDefs []models.OpenAIFunction
		for _, tool := range req.Tools {
			fn := tool.Function
			if fn.Name == "" {
				fn.Name = tool.Name
			}
			if fn.Description == "" {
				fn.Description = tool.Description
			}
			if fn.Parameters == nil {
				fn.Parameters = tool.Parameters
			}
			if fn.Parameters == nil {
				fn.Parameters = map[string]any{}
			}

			toolDefs = append(toolDefs, fn)
		}

		if len(toolDefs) > 0 {
			constraint := BuildToolChoiceInstruction(req.ToolChoice)
			// Compression never discards the JSON Schema: it sheds cosmetic
			// keywords and (at the last level) deep properties while keeping
			// type/required/enum/items/additionalProperties. See schema.go.
			defsJSON, level := CompressToolDefinitions(toolDefs, opts.MaxSchemaBytes)
			if level != SchemaKeepAll {
				log.Printf("Tool defs compressed (level=%s, %d bytes, %d tools)",
					level, len(defsJSON), len(toolDefs))
			}
			parts = append(parts, fmt.Sprintf(
				"# Tool Use\n\n"+
					"You are running inside an agent runtime that executes the tools below on the user's machine. "+
					"You do NOT execute them yourself; you emit a call and the runtime returns the result.\n\n"+
					"Each tool's `parameters` is a JSON Schema; argument names and types MUST conform to it.\n"+
					"Call format:\n"+
					"```tool_call\n{\"name\": \"func_name\", \"arguments\": {...}}\n```\n"+
					"When calling tools, output ONLY the tool_call block(s).\n\n"+
					"A `[Tool result for <name>]` message is the real output of a call you already made. "+
					"Treat it as authoritative fact and continue the task from it. "+
					"Never claim you lack access to a tool: every tool listed here is available.\n\n"+
					"Available tools:\n%s%s",
				string(defsJSON),
				constraint,
			))
		}
	}

	for _, msg := range req.Messages {
		role := msg.Role
		if role == "" {
			role = "user"
		}

		var contentStr string
		if strContent, ok := msg.Content.(string); ok {
			contentStr = strContent
		} else if contentList, ok := msg.Content.([]any); ok {
			var textParts []string
			for _, item := range contentList {
				if mapItem, ok := item.(map[string]any); ok {
					iType, _ := mapItem["type"].(string)
					if iType == "text" || iType == "input_text" || iType == "output_text" {
						if t, ok := mapItem["text"].(string); ok {
							textParts = append(textParts, t)
						}
					} else if img, ok := ImageFromPart(mapItem); ok {
						images = append(images, img)
						textParts = append(textParts, "[Image attached]")
					}
				} else if s, ok := item.(string); ok && s != "" {
					textParts = append(textParts, s)
				}
			}
			contentStr = strings.Join(textParts, " ")
		}

		switch role {
		case "system", "developer":
			parts = append(parts, fmt.Sprintf("[System instruction]: %s", contentStr))
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				var tcStrs []string
				for _, tc := range msg.ToolCalls {
					argsStr := tc.Function.Arguments
					if argsStr == "" {
						argsStr = "{}"
					}
					// Echo the id back so the model can pair the result with the
					// call. Without it, parallel calls with the same tool name
					// become ambiguous.
					tcStrs = append(tcStrs, buildToolCallBlock(tc.ID, tc.Function.Name, argsStr))
				}
				parts = append(parts, fmt.Sprintf("[Assistant]: %s\n%s", contentStr, strings.Join(tcStrs, "\n")))
			} else {
				parts = append(parts, fmt.Sprintf("[Assistant]: %s", contentStr))
			}
		case "tool", "function":
			// The model needs the tool NAME to make sense of the result; an
			// opaque call id tells it nothing. Standard OpenAI clients send only
			// tool_call_id, so resolve the name from the matching assistant
			// tool_calls entry recorded earlier in the conversation.
			name := msg.Name
			if name == "" {
				name = callNames[msg.ToolCallID]
			}
			var label string
			switch {
			case name != "" && msg.ToolCallID != "":
				// Best case: the model sees both what produced the data and
				// which call it belongs to, so parallel calls stay distinct.
				label = fmt.Sprintf("%s (call id %s)", name, msg.ToolCallID)
			case name != "":
				label = name
			case msg.ToolCallID != "":
				// The id was never seen in an assistant turn (truncated history
				// or client-generated id). Say so instead of presenting an
				// opaque id as if it were a tool name.
				label = fmt.Sprintf("%s, tool name unknown", msg.ToolCallID)
			default:
				label = "unknown tool"
			}
			parts = append(parts, fmt.Sprintf("[Tool result for %s]: %s", label, contentStr))
		default:
			if contentStr != "" {
				parts = append(parts, contentStr)
			}
		}
	}

	return strings.Join(parts, "\n\n"), images, nil
}

// buildToolCallBlock renders a single tool call in the format the model is
// told to reproduce. JSON-encoding the name avoids quote-injection.
func buildToolCallBlock(id, name, argsJSON string) string {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	var payload string
	if id != "" {
		payload = fmt.Sprintf(`{"name": %q, "id": %q, "arguments": %s}`, name, id, argsJSON)
	} else {
		payload = fmt.Sprintf(`{"name": %q, "arguments": %s}`, name, argsJSON)
	}
	return "```tool_call\n" + payload + "\n```"
}

// ParseToolCalls extracts tool calls from model output.
//
// Models are told to emit ```tool_call {"name":..., "arguments":...} ``` blocks,
// but output in the wild is messier: missing closing fences, bare JSON, prose
// mixed in, nested braces and backticks inside argument strings, or multiple
// blocks per answer. extractPayloads handles discovery; this function maps each
// payload to a structurally valid OpenAI tool call and returns the residual
// text with all call blocks stripped.
//
// Unparseable payloads are dropped from the call list but still removed from
// the text so the user never sees raw tool syntax.
func ParseToolCalls(text string) (string, []models.OpenAIToolCall) {
	clean, payloads := extractPayloads(text)
	if len(payloads) == 0 {
		clean = text
		// Some models emit a single bare JSON object with no tag at all when
		// the only instruction is a schema. Only accept it when it clearly
		// looks like a call.
		if name, args, _, id, ok := parseCallPayload(strings.TrimSpace(text)); ok &&
			strings.HasPrefix(strings.TrimSpace(text), "{") {
			return "", []models.OpenAIToolCall{newToolCall(id, name, args)}
		}
		return clean, nil
	}

	var toolCalls []models.OpenAIToolCall
	for _, payload := range payloads {
		name, argsString, _, id, ok := parseCallPayload(payload)
		if !ok {
			continue
		}
		toolCalls = append(toolCalls, newToolCall(id, name, argsString))
	}

	return clean, toolCalls
}

// newToolCall builds a validated OpenAI tool call, generating an id when the
// model did not supply one.
func newToolCall(id, name, argsJSON string) models.OpenAIToolCall {
	if id == "" {
		id = newCallID()
	}
	if argsJSON == "" {
		argsJSON = "{}"
	}
	return models.OpenAIToolCall{
		ID:   id,
		Type: "function",
		Function: models.OpenAIToolCallFunction{
			Name:      name,
			Arguments: argsJSON,
		},
	}
}

// BuildToolCallsSummary renders tool calls as a short human-readable list for
// prompts where a full block would be redundant.
func BuildToolCallsSummary(calls []models.OpenAIToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	for i, tc := range calls {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tc.Function.Name)
		b.WriteString("(")
		b.WriteString(tc.Function.Arguments)
		b.WriteString(")")
	}
	return b.String()
}
