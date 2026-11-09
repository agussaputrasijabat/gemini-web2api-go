package server

import (
	"net/http"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/format"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// This file owns the OpenAI chat.completion.chunk shape for streams that carry
// tool calls. Gemini's web endpoint has no native function calling and streams
// plain text, so tool calls can only be detected after the whole answer is in.
//
// Trade-off, stated explicitly because it shapes the code below:
//
//	Detecting a tool call incrementally would require a streaming JSON parser
//	that buffers potential fence boundaries, and it would still emit partial
//	`function.arguments` fragments for text that may turn out to be prose. That
//	is fragile: one mis-detected fence mid-answer corrupts the client's
//	accumulator.
//
//	We therefore use a hybrid strategy:
//	  - Text that cannot be part of a tool block is streamed immediately.
//	  - Once an opening fence is seen, everything from that point is buffered.
//	  - At end of stream, the buffer is parsed and, when calls are found, all
//	    of them are emitted as indexed delta.tool_calls chunks before the final
//	    finish_reason: "tool_calls" chunk.
//
//	This keeps time-to-first-token for normal chat and produces a
//	protocol-valid tool call stream for agent traffic.

// sseChatStreamer emits chat.completion.chunk events for one completion.
type sseChatStreamer struct {
	w       http.ResponseWriter
	id      string
	created int64
	model   string
}

func newSSEChatStreamer(w http.ResponseWriter, id, model string) *sseChatStreamer {
	return &sseChatStreamer{
		w:       w,
		id:      id,
		created: time.Now().Unix(),
		model:   model,
	}
}

func (s *sseChatStreamer) baseChunk(delta *models.OpenAIDeltaMessage, finish *string) models.OpenAIChatResponse {
	return models.OpenAIChatResponse{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []models.OpenAIChoice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: finish,
			},
		},
	}
}

// Role emits the leading chunk carrying delta.role, as the OpenAI protocol
// shows for every stream.
func (s *sseChatStreamer) Role() error {
	return writeSSEData(s.w, s.baseChunk(&models.OpenAIDeltaMessage{Role: "assistant"}, nil))
}

// Content emits one text delta.
func (s *sseChatStreamer) Content(delta string) error {
	if delta == "" {
		return nil
	}
	return writeSSEData(s.w, s.baseChunk(&models.OpenAIDeltaMessage{Content: delta}, nil))
}

// ToolCalls emits tool calls as indexed deltas, then the terminal chunk with
// finish_reason "tool_calls".
//
// Each call gets a distinct index and a unique id. Name and the full argument
// string are sent in a single delta per call: clients accumulate by index, so a
// whole-value delta is simpler and cannot desynchronise. Sending arguments in
// fragments is also legal, but adds no value when the value is already known.
func (s *sseChatStreamer) ToolCalls(calls []models.OpenAIToolCall) error {
	for i, tc := range calls {
		id := tc.ID
		if id == "" {
			id = newStreamCallID()
		}
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		chunk := s.baseChunk(&models.OpenAIDeltaMessage{
			ToolCalls: []models.OpenAIToolCallDelta{
				{
					Index: i,
					ID:    id,
					Type:  "function",
					Function: models.OpenAIToolCallFuncDelta{
						Name:      tc.Function.Name,
						Arguments: args,
					},
				},
			},
		}, nil)
		if err := writeSSEData(s.w, chunk); err != nil {
			return err
		}
	}

	finish := "tool_calls"
	return writeSSEData(s.w, s.baseChunk(&models.OpenAIDeltaMessage{}, &finish))
}

// Finish emits the terminal chunk. When calls are present the finish reason is
// "tool_calls", never "stop".
func (s *sseChatStreamer) Finish(calls []models.OpenAIToolCall) error {
	if len(calls) > 0 {
		return s.ToolCalls(calls)
	}
	finish := "stop"
	if err := writeSSEData(s.w, s.baseChunk(&models.OpenAIDeltaMessage{}, &finish)); err != nil {
		return err
	}
	return nil
}

// newStreamCallID mirrors the call_<hex> id shape used by the format package.
func newStreamCallID() string {
	return "call_" + format.RandHex(12)
}
