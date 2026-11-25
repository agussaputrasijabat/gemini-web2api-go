package server

import (
	"context"
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

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil || len(bodyBytes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	var req models.OpenAIChatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	modelStr := req.Model
	if modelStr == "" {
		modelStr = a.Cfg.DefaultModel
	}

	resolved, err := models.ResolveWithOptions(modelStr, a.Cfg.DefaultModel, models.ResolveOptions{
		ReasoningEffort: requestEffort(req),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	setModelHeaders(w, resolved)

	if req.ToolChoice == nil {
		req.ToolChoice = "auto"
	}

	toolMode := format.ToolChoiceMode(req.ToolChoice)
	// Tools are active unless the client explicitly disabled them. Streaming no
	// longer bypasses tool handling: agent clients almost always send
	// stream:true together with tools, so the old
	// `if req.Stream && len(req.Tools)==0` shortcut silently dropped every tool
	// call for those clients.
	toolsActive := len(req.Tools) > 0 && toolMode != "none"

	prompt, images, err := format.MessagesToPromptWithOptions(req, a.toolOptions())
	if err != nil || strings.TrimSpace(prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "empty prompt"}})
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

	cid := fmt.Sprintf("chatcmpl-%s", format.RandHex(12))

	if req.Stream {
		a.streamChat(r.Context(), w, req, prompt, fileRefs, gemOpt, resolved.Name, cid, toolsActive)
		return
	}

	text, err := a.Gem.Generate(r.Context(), prompt, gemOpt, fileRefs)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": fmt.Sprintf("upstream error: %v", err)}})
		return
	}

	var toolCalls []models.OpenAIToolCall
	if toolsActive && text != "" {
		text, toolCalls = format.ParseToolCalls(text)
	}

	msg := models.OpenAIMessage{
		Role:    "assistant",
		Content: text,
	}
	if text == "" {
		msg.Content = nil
	}

	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}

	promptTokens := len(prompt) / 4
	completionTokens := len(text) / 4
	resp := models.OpenAIChatResponse{
		ID:      cid,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resolved.Name,
		Choices: []models.OpenAIChoice{
			{
				Index:        0,
				Message:      &msg,
				FinishReason: &finish,
			},
		},
		Usage: &models.OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// streamChat serves chat.completion.chunk SSE, including tool calls.
//
// See stream_tools.go for the buffering trade-off. In short: text streams live
// until a tool-call fence appears, everything from the fence onward is buffered,
// and at end of stream the buffer is parsed. If parsing finds no call, the
// buffered text is flushed as ordinary content, so a false positive costs
// latency but never loses output.
func (a *App) streamChat(
	ctx context.Context,
	w http.ResponseWriter,
	req models.OpenAIChatRequest,
	prompt string,
	fileRefs []string,
	gemOpt gemini.ModelOptions,
	modelName, cid string,
	toolsActive bool,
) {
	if !startSSE(w) {
		return
	}

	streamer := newSSEChatStreamer(w, cid, modelName)
	if err := streamer.Role(); err != nil {
		return
	}

	gate := newToolStreamGate(toolsActive)
	sentContent := false
	emitErr := a.Gem.GenerateStream(ctx, prompt, gemOpt, fileRefs, func(delta string) error {
		return gate.Feed(delta, func(passthrough string) error {
			if err := streamer.Content(passthrough); err != nil {
				return err
			}
			sentContent = true
			return nil
		})
	})

	if emitErr != nil {
		a.Logf("Chat stream error: %v", emitErr)
		// Flush whatever we buffered so the client is not left hanging, then
		// terminate the stream without a finish_reason claim we cannot honour.
		if flushed := gate.Pending(); flushed != "" {
			if err := streamer.Content(flushed); err == nil {
				sentContent = true
			}
		}
		if !sentContent {
			// Nothing reached the client yet, so surface the upstream failure
			// instead of a 200 stream that looks like an empty answer.
			_ = writeSSEData(w, map[string]any{
				"error": map[string]any{"message": fmt.Sprintf("upstream error: %v", emitErr)},
			})
			_ = writeSSEDone(w)
			return
		}
		stop := "stop"
		_ = writeSSEData(w, streamer.baseChunk(&models.OpenAIDeltaMessage{}, &stop))
		_ = writeSSEDone(w)
		return
	}

	buffered := gate.Pending()
	if buffered != "" {
		clean, calls := format.ParseToolCalls(buffered)
		if len(calls) > 0 {
			if clean != "" {
				if err := streamer.Content(clean); err != nil {
					return
				}
			}
			if err := streamer.ToolCalls(calls); err != nil {
				return
			}
			_ = writeSSEDone(w)
			return
		}
		// No usable call. The buffer may still hold an unparseable
		// ```tool_call block (truncated JSON, for instance). Emit the cleaned
		// text so the raw marker never reaches the client, matching what the
		// non-streaming path does. Clean equals buffered when no fence exists.
		if err := streamer.Content(clean); err != nil {
			return
		}
	}

	if err := streamer.Finish(nil); err != nil {
		return
	}
	_ = writeSSEDone(w)
}

// toolStreamGate separates streamable prose from buffered tool-call candidates.
type toolStreamGate struct {
	enabled bool
	buf     string
	// holding becomes true once a tool-call marker was seen; from then on all
	// further deltas are buffered instead of streamed.
	holding bool
}

func newToolStreamGate(enabled bool) *toolStreamGate {
	return &toolStreamGate{enabled: enabled}
}

// toolMarkers are the byte sequences that begin a tool-call block. Matching is
// case-insensitive because models vary the casing.
var toolMarkers = []string{
	"```tool_call",
	"```tool_calls",
	"```function_call",
	"```function_calls",
}

// Feed consumes one upstream delta. Text that definitely cannot start a tool
// block is passed to emit immediately; anything else is buffered.
func (g *toolStreamGate) Feed(delta string, emit func(string) error) error {
	if !g.enabled {
		return emit(delta)
	}

	g.buf += delta
	if g.holding {
		return nil
	}

	lower := strings.ToLower(g.buf)
	if idx := indexAnyMarker(lower); idx >= 0 {
		// Keep the marker and everything after it for end-of-stream parsing.
		head := g.buf[:idx]
		g.buf = g.buf[idx:]
		g.holding = true
		if head != "" {
			return emit(head)
		}
		return nil
	}

	// No marker yet. Hold back a tail that could still grow into a marker so a
	// fence split across two deltas is not streamed in fragments.
	keep := markerPrefixSuffixLen(lower)
	if keep > len(g.buf) {
		keep = len(g.buf)
	}
	if keep < len(g.buf) {
		head := g.buf[:len(g.buf)-keep]
		g.buf = g.buf[len(g.buf)-keep:]
		if head != "" {
			return emit(head)
		}
	}
	return nil
}

// Pending returns the buffered remainder that has not been emitted.
func (g *toolStreamGate) Pending() string {
	return g.buf
}

// indexAnyMarker returns the earliest marker index in s, or -1.
func indexAnyMarker(s string) int {
	best := -1
	for _, m := range toolMarkers {
		if idx := strings.Index(s, m); idx >= 0 && (best < 0 || idx < best) {
			best = idx
		}
	}
	return best
}

// markerPrefixSuffixLen returns the length of the longest suffix of s that is
// also a proper prefix of some marker.
func markerPrefixSuffixLen(s string) int {
	best := 0
	for _, m := range toolMarkers {
		max := len(m) - 1
		if max > len(s) {
			max = len(s)
		}
		for n := max; n > best; n-- {
			if strings.HasSuffix(s, m[:n]) {
				best = n
				break
			}
		}
	}
	return best
}
