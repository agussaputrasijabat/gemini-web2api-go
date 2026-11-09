package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/format"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// toolCallFromMap converts the loose map form produced by
// ResponsesInputToMessages back into the typed tool call.
func toolCallFromMap(m map[string]any) models.OpenAIToolCall {
	tc := models.OpenAIToolCall{}
	tc.ID, _ = m["id"].(string)
	tc.Type, _ = m["type"].(string)
	if tc.Type == "" {
		tc.Type = "function"
	}
	if fn, ok := m["function"].(map[string]any); ok {
		tc.Function.Name, _ = fn["name"].(string)
		tc.Function.Arguments, _ = fn["arguments"].(string)
	}
	if tc.Function.Arguments == "" {
		tc.Function.Arguments = "{}"
	}
	return tc
}

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil || len(bodyBytes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	var req map[string]any
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	modelStr, _ := req["model"].(string)
	if modelStr == "" {
		modelStr = a.Cfg.DefaultModel
	}

	resolved, err := models.ResolveWithOptions(modelStr, a.Cfg.DefaultModel, models.ResolveOptions{
		ReasoningEffort: responsesEffort(req),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	setModelHeaders(w, resolved)

	inputRaw := req["input"]
	instructions, _ := req["instructions"].(string)

	messages, err := format.ResponsesInputToMessages(inputRaw, instructions)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid input structure"}})
		return
	}

	var reqMessages []models.OpenAIMessage
	for _, m := range messages {
		role, _ := m["role"].(string)
		msg := models.OpenAIMessage{Role: role, Content: m["content"]}

		// Results must reach MessagesToPrompt with the call id intact; without
		// it a role:"tool" turn loses which call it answers.
		if id, ok := m["tool_call_id"].(string); ok {
			msg.ToolCallID = id
		}
		if name, ok := m["name"].(string); ok {
			msg.Name = name
		}
		if rawCalls, ok := m["tool_calls"].([]map[string]any); ok {
			for _, rc := range rawCalls {
				msg.ToolCalls = append(msg.ToolCalls, toolCallFromMap(rc))
			}
		}
		reqMessages = append(reqMessages, msg)
	}

	var reqTools []models.OpenAITool
	if toolsRaw, ok := req["tools"].([]any); ok {
		for _, t := range toolsRaw {
			if tm, ok := t.(map[string]any); ok {
				var fn models.OpenAIFunction
				if f, ok := tm["function"].(map[string]any); ok {
					fn.Name, _ = f["name"].(string)
					fn.Description, _ = f["description"].(string)
					fn.Parameters = f["parameters"]
				} else {
					fn.Name, _ = tm["name"].(string)
					fn.Description, _ = tm["description"].(string)
					fn.Parameters = tm["parameters"]
				}
				reqTools = append(reqTools, models.OpenAITool{Function: fn})
			}
		}
	}

	toolChoice := req["tool_choice"]
	if toolChoice == nil {
		toolChoice = "auto"
	}

	chatReq := models.OpenAIChatRequest{
		Messages:   reqMessages,
		Tools:      reqTools,
		ToolChoice: toolChoice,
	}

	prompt, images, err := format.MessagesToPromptWithOptions(chatReq, a.toolOptions())
	if err != nil || strings.TrimSpace(prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "empty input"}})
		return
	}

	// Upload images to Gemini storage.
	var fileRefs []string
	if len(images) > 0 {
		fileRefs, err = a.uploadImages(images)
		if err != nil {
			a.Logf("Image upload error: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": fmt.Sprintf("upstream error: %v", err)}})
			return
		}
	}

	gemOpt := gemini.ModelOptions{
		ModelName:   resolved.Name,
		ModelID:     resolved.Mode,
		ThinkMode:   resolved.Think,
		ModelHeader: resolved.Header,
		Extra:       resolved.Extra,
	}

	text, err := a.Gem.Generate(r.Context(), prompt, gemOpt, fileRefs)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": fmt.Sprintf("upstream error: %v", err)}})
		return
	}

	toolMode := format.ToolChoiceMode(toolChoice)
	isToolNone := toolMode == "none"

	var toolCalls []models.OpenAIToolCall
	if len(reqTools) > 0 && text != "" && !isToolNone {
		text, toolCalls = format.ParseToolCalls(text)
	}

	rid := fmt.Sprintf("resp_%s", format.RandHex(16))
	mid := fmt.Sprintf("msg_%s", format.RandHex(12))

	outputItems := format.BuildResponseOutput(text, toolCalls, mid)

	stream, _ := req["stream"].(bool)
	promptTokens := len(prompt) / 4
	outputTokens := len(text) / 4

	if stream {
		if !startSSE(w) {
			return
		}

		seqNum := 0
		var streamErr error
		emit := func(eventType string, fields map[string]any) {
			if streamErr != nil {
				return
			}
			seqNum++
			event := map[string]any{
				"type":            eventType,
				"sequence_number": seqNum,
				"response_id":     rid,
			}
			for k, v := range fields {
				event[k] = v
			}
			streamErr = writeSSEEvent(w, eventType, event)
		}

		usage := map[string]any{
			"input_tokens":  promptTokens,
			"output_tokens": outputTokens,
			"total_tokens":  promptTokens + outputTokens,
		}
		baseResp := map[string]any{
			"id":         rid,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      resolved.Name,
		}
		withStatus := func(status string, output any, use map[string]any) map[string]any {
			r := map[string]any{
				"id":         baseResp["id"],
				"object":     baseResp["object"],
				"created_at": baseResp["created_at"],
				"model":      baseResp["model"],
				"status":     status,
				"output":     output,
				"usage":      use,
			}
			return r
		}
		emit("response.created", map[string]any{
			"response": withStatus("in_progress", []any{}, nil),
		})
		emit("response.in_progress", map[string]any{
			"response": withStatus("in_progress", []any{}, nil),
		})

		for itemIdx, item := range outputItems {
			iType, _ := item["type"].(string)
			itemID, _ := item["id"].(string)

			switch iType {
			case "function_call":
				pending := map[string]any{
					"type":      "function_call",
					"id":        item["id"],
					"call_id":   item["call_id"],
					"name":      item["name"],
					"arguments": "",
					"status":    "in_progress",
				}
				emit("response.output_item.added", map[string]any{
					"output_index": itemIdx,
					"item":         pending,
				})

				args, _ := item["arguments"].(string)
				emit("response.function_call_arguments.delta", map[string]any{
					"item_id":      itemID,
					"output_index": itemIdx,
					"delta":        args,
				})
				emit("response.function_call_arguments.done", map[string]any{
					"item_id":      itemID,
					"output_index": itemIdx,
					"arguments":    args,
				})
				emit("response.output_item.done", map[string]any{
					"output_index": itemIdx,
					"item":         item,
				})
			case "message":
				pending := map[string]any{
					"type":    "message",
					"id":      item["id"],
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				}
				emit("response.output_item.added", map[string]any{
					"output_index": itemIdx,
					"item":         pending,
				})

				if content, ok := item["content"].([]map[string]any); ok {
					for ci, cp := range content {
						eventFields := map[string]any{
							"item_id":       itemID,
							"output_index":  itemIdx,
							"content_index": ci,
						}
						emit("response.content_part.added", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"part": map[string]any{
								"type":        "output_text",
								"text":        "",
								"annotations": []any{},
							},
						})
						cpText, _ := cp["text"].(string)
						emit("response.output_text.delta", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"delta":         cpText,
						})
						emit("response.output_text.done", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"text":          cpText,
						})
						emit("response.content_part.done", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"part":          cp,
						})
					}
				}

				emit("response.output_item.done", map[string]any{
					"output_index": itemIdx,
					"item":         item,
				})
			}
		}

		emit("response.completed", map[string]any{
			"response": withStatus("completed", outputItems, usage),
		})
		// The Responses API terminates the SSE stream after
		// response.completed; unlike chat.completions it has no [DONE] sentinel.
	} else {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         rid,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      resolved.Name,
			"output":     outputItems,
			"usage": map[string]any{
				"input_tokens":  promptTokens,
				"output_tokens": outputTokens,
				"total_tokens":  promptTokens + outputTokens,
			},
		})
	}
}
