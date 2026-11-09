package format

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// ResponsesInputToMessages converts a Responses API `input` value into
// OpenAI-style chat messages.
//
// The Responses API accepts a flat list of input items where function calls and
// their results are siblings, not nested inside an assistant message:
//
//	{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{...}"}
//	{"type":"function_call_output","call_id":"call_1","output":"{\"temp\":30}"}
//
// Codex CLI sends exactly that shape. The previous implementation only looked
// for function_call inside an assistant message's content array and matched
// function_call_output by name, so top-level items were silently flattened into
// plain user text and the tool roundtrip broke.
func ResponsesInputToMessages(input any, instructions string) ([]map[string]any, error) {
	var messages []map[string]any

	if instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	// assistantToolCalls tracks the tool call batched into the most recent
	// assistant message so consecutive function_call items merge into a single
	// assistant turn (OpenAI emits them as one message with several calls).
	var lastAssistant map[string]any

	flushAssistant := func() {
		lastAssistant = nil
	}

	appendToolCall := func(callID, name, arguments string) {
		if arguments == "" {
			arguments = "{}"
		}
		if callID == "" {
			callID = newCallID()
		}
		if lastAssistant == nil {
			lastAssistant = map[string]any{
				"role":    "assistant",
				"content": nil,
			}
			messages = append(messages, lastAssistant)
		}
		existing, _ := lastAssistant["tool_calls"].([]map[string]any)
		existing = append(existing, map[string]any{
			"id":   callID,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": arguments,
			},
		})
		lastAssistant["tool_calls"] = existing
	}

	if strInput, ok := input.(string); ok {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": strInput,
		})
	} else if listInput, ok := input.([]any); ok {
		for _, item := range listInput {
			if strItem, ok := item.(string); ok {
				flushAssistant()
				messages = append(messages, map[string]any{
					"role":    "user",
					"content": strItem,
				})
				continue
			}
			mapItem, ok := item.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := mapItem["type"].(string)
			itemRole, _ := mapItem["role"].(string)

			switch itemType {
			case "function_call":
				// Codex/Responses style: top-level call item.
				callID, _ := mapItem["call_id"].(string)
				if callID == "" {
					callID, _ = mapItem["id"].(string)
				}
				name, _ := mapItem["name"].(string)
				args, _ := mapItem["arguments"].(string)
				appendToolCall(callID, name, args)
				continue

			case "function_call_output":
				// The tool result must reference the assistant call by id, or
				// the model cannot pair it with the request it made.
				callID, _ := mapItem["call_id"].(string)
				output := stringifyResponsesOutput(mapItem["output"])
				flushAssistant()
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      output,
				})
				continue
			}

			if itemRole == "assistant" || (itemType == "message" && itemRole == "assistant") {
				flushAssistant()
				rawContent := mapItem["content"]
				var textAcc string
				var tcList []map[string]any

				if cpList, ok := rawContent.([]any); ok {
					for _, cp := range cpList {
						if cpMap, ok := cp.(map[string]any); ok {
							cpType, _ := cpMap["type"].(string)
							switch cpType {
							case "output_text", "text", "input_text":
								t, _ := cpMap["text"].(string)
								textAcc += t
							case "function_call":
								tcList = append(tcList, cpMap)
							}
						}
					}
				} else if strCp, ok := rawContent.(string); ok {
					textAcc = strCp
				}

				m := map[string]any{
					"role":    "assistant",
					"content": textAcc,
				}
				if textAcc == "" {
					m["content"] = nil
				}

				if len(tcList) > 0 {
					var toolCalls []map[string]any
					for i, tc := range tcList {
						callID, _ := tc["call_id"].(string)
						if callID == "" {
							callID, _ = tc["id"].(string)
						}
						if callID == "" {
							callID = fmt.Sprintf("call_%d", i)
						}
						name, _ := tc["name"].(string)
						args, _ := tc["arguments"].(string)
						if args == "" {
							args = "{}"
						}
						toolCalls = append(toolCalls, map[string]any{
							"id":   callID,
							"type": "function",
							"function": map[string]any{
								"name":      name,
								"arguments": args,
							},
						})
					}
					m["tool_calls"] = toolCalls
				}
				messages = append(messages, m)
				continue
			}

			// Everything else is a user/system turn.
			flushAssistant()
			role := itemRole
			if role == "" {
				role = "user"
			}
			// Preserve raw content (list or string) so downstream
			// MessagesToPrompt can extract image parts. Flattening to
			// a string here would silently drop image data.
			rawContent := mapItem["content"]
			if rawContent == nil {
				rawContent = ""
			}
			if cList, ok := rawContent.([]any); ok {
				// Normalize text-only lists to a joined string for
				// cleaner prompts, but keep the list when it contains
				// image parts.
				hasImage := false
				for _, c := range cList {
					if cMap, ok := c.(map[string]any); ok {
						if _, ok := ImageFromPart(cMap); ok {
							hasImage = true
							break
						}
					}
				}
				if !hasImage {
					var parts []string
					for _, c := range cList {
						if cMap, ok := c.(map[string]any); ok {
							cType, _ := cMap["type"].(string)
							if cType == "text" || cType == "input_text" || cType == "output_text" {
								if txt, ok := cMap["text"].(string); ok {
									parts = append(parts, txt)
								}
							}
						} else if s, ok := c.(string); ok {
							parts = append(parts, s)
						}
					}
					rawContent = strings.Join(parts, " ")
				}
			}

			messages = append(messages, map[string]any{
				"role":    role,
				"content": rawContent,
			})
		}
	}

	return messages, nil
}

// stringifyResponsesOutput normalizes a function_call_output `output` field.
// It may be a plain string or structured JSON (some clients send an object).
func stringifyResponsesOutput(output any) string {
	switch v := output.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		buf, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(buf)
	}
}

// BuildResponseOutput builds the `output` array for a Responses API reply.
//
// Ordering follows the wire contract clients aggregate by output_index: a
// function_call item must always carry `call_id`, `name` and `arguments`, with
// `id` as the item id (not the call id) so the two are not conflated.
func BuildResponseOutput(text string, toolCalls []models.OpenAIToolCall, msgID string) []map[string]any {
	var output []map[string]any

	for _, tc := range toolCalls {
		callID := tc.ID
		if callID == "" {
			callID = newCallID()
		}
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%s", RandHex(12)),
			"call_id":   callID,
			"name":      tc.Function.Name,
			"arguments": args,
			"status":    "completed",
		})
	}

	if text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     msgID,
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]any{
				{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				},
			},
		})
	}

	return output
}
