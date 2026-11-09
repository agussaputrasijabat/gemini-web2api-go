package models

type OpenAIChatRequest struct {
	Model      string          `json:"model"`
	Messages   []OpenAIMessage `json:"messages"`
	Tools      []OpenAITool    `json:"tools"`
	ToolChoice any             `json:"tool_choice"`
	Stream     bool            `json:"stream"`

	// ReasoningEffort accepts none/minimal/low/medium/high/xhigh (OpenAI-style).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// Reasoning carries the newer nested form, e.g. {"effort":"high"}.
	Reasoning *OpenAIReasoning `json:"reasoning,omitempty"`
}

// OpenAIReasoning is the nested reasoning config some clients send.
type OpenAIReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type OpenAIMessage struct {
	Role      string           `json:"role"`
	Content   any              `json:"content"`
	Name      string           `json:"name,omitempty"`
	ToolCalls []OpenAIToolCall `json:"tool_calls,omitempty"`

	// ToolCallID links a role:"tool" message back to the assistant tool call
	// that produced it. Required for correct multi-turn tool roundtrips.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`

	// Flat tool declarations fallback
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type OpenAIFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type OpenAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function OpenAIToolCallFunction `json:"function"`
}

type OpenAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

// OpenAIToolCallDelta is the streaming form of a tool call. Unlike
// OpenAIToolCall it carries the assistant-message array index, because the
// OpenAI stream protocol identifies each tool call by position and clients
// accumulate `function.arguments` fragments by that index.
type OpenAIToolCallDelta struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function OpenAIToolCallFuncDelta `json:"function"`
}

// OpenAIToolCallFuncDelta is the streamed fragment of a function call.
// Name is only sent on the first fragment for an index; Arguments may be sent
// whole or split across chunks.
type OpenAIToolCallFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// OpenAIDeltaMessage is the streamed counterpart of OpenAIMessage. Content is
// always a plain string delta, and tool calls use the indexed delta shape.
type OpenAIDeltaMessage struct {
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []OpenAIToolCallDelta `json:"tool_calls,omitempty"`
}

type OpenAIChoice struct {
	Index        int                 `json:"index"`
	Message      *OpenAIMessage      `json:"message,omitempty"`
	Delta        *OpenAIDeltaMessage `json:"delta,omitempty"`
	FinishReason *string             `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
