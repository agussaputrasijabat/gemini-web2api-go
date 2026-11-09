package models

type GoogleGenerateRequest struct {
	Contents          []GoogleContent      `json:"contents"`
	Tools             []GoogleTool         `json:"tools"`
	ToolConfig        *GoogleToolConfig    `json:"toolConfig"`
	SystemInstruction *GoogleContent       `json:"systemInstruction"`
	GenerationConfig  *GoogleGenerationCfg `json:"generationConfig,omitempty"`
}

// GoogleGenerationCfg only carries the fields this proxy consumes.
type GoogleGenerationCfg struct {
	ThinkingConfig *GoogleThinkingConfig `json:"thinkingConfig,omitempty"`
}

// GoogleThinkingConfig mirrors the native Gemini API shape.
// thinkingLevel is none/minimal/low/medium/high; thinkingBudget is the legacy
// integer knob and is accepted but ignored.
type GoogleThinkingConfig struct {
	ThinkingLevel  string `json:"thinkingLevel,omitempty"`
	ThinkingBudget *int   `json:"thinkingBudget,omitempty"`
}

type GoogleContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GooglePart `json:"parts"`
}

type GooglePart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *GoogleInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *GoogleFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GoogleFunctionCallResp `json:"functionResponse,omitempty"`

	// ThoughtSignature is an opaque token the native API issues alongside a
	// functionCall. It must be echoed back on the same part in follow-up turns
	// for models that validate it. A prompt-based proxy cannot replay it into
	// the upstream web endpoint, so it is preserved on the way out only.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type GoogleInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GoogleFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	// ID pairs a functionCall with its functionResponse (Gemini 3.x).
	ID string `json:"id,omitempty"`
}

type GoogleFunctionCallResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response,omitempty"`
	// ID must equal the id of the functionCall being answered.
	ID string `json:"id,omitempty"`
	// Scheduling is echoed back by some clients; preserved for parity.
	Scheduling string `json:"scheduling,omitempty"`

	// ThoughtSignature mirrors GooglePart.ThoughtSignature for clients that
	// nest it inside the response object.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type GoogleTool struct {
	FunctionDeclarations []GoogleFunctionDeclaration `json:"functionDeclarations"`
}

type GoogleFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type GoogleToolConfig struct {
	FunctionCallingConfig *GoogleFunctionCallingConfig `json:"functionCallingConfig"`
}

type GoogleFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type GoogleGenerateResponse struct {
	Candidates    []GoogleCandidate    `json:"candidates"`
	UsageMetadata *GoogleUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string               `json:"modelVersion,omitempty"`
}

type GoogleCandidate struct {
	Index        int            `json:"index"`
	Content      *GoogleContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
}

type GoogleUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}
