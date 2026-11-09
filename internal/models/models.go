package models

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
)

// Model represents a Gemini backend routing configuration.
// Mode corresponds to the Gemini upstream mode ID, Think specifies the default reasoning depth,
// and Extra contains index-based overrides for Gemini's sparse RPC request array.
type Model struct {
	Mode   int         `json:"mode"`
	Think  int         `json:"think"`
	Desc   string      `json:"desc"`
	Header string      `json:"header,omitempty"`
	Extra  map[int]any `json:"extra,omitempty"`
}

var MODELS = map[string]Model{
	"gemini-4-argon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Frontier model for complex tasks (Gemini 4 Argon, requires Google AI Pro cookie)",
	},
	"argon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Alias for gemini-4-argon",
	},
	"gemini-4": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Alias for gemini-4-argon",
	},
	"gemini-3.8-flash": {
		Mode:  1,
		Think: 4,
		Desc:  "Latest all-around model (Gemini 3.8 Flash)",
	},
	"gemini-3.7-flash": {
		Mode:  1,
		Think: 4,
		Desc:  "All-around model (Gemini 3.7 Flash)",
	},
	"gemini-3.6-flash": {
		Mode:  1,
		Think: 4,
		Desc:  "All-around model (Gemini 3.6 Flash)",
	},
	"gemini-3.5-flash": {
		Mode:  1,
		Think: 4,
		Desc:  "Alias for gemini-3.6-flash (backend upgraded)",
	},
	"gemini-3.5-flash-thinking": {
		Mode:  2,
		Think: 0,
		Desc:  "Deep thinking mode, longest output (~20k chars)",
	},
	"gemini-3.1-pro": {
		Mode:  3,
		Think: 4,
		Desc:  "Pro model (requires cookie for real routing)",
	},
	"gemini-3.1-pro-enhanced": {
		Mode:  3,
		Think: 4,
		Desc:  "Pro with enhanced output (experimental)",
		Extra: map[int]any{31: 2, 80: 3},
	},
	"gemini-auto": {
		Mode:  4,
		Think: 4,
		Desc:  "Auto model selection",
	},
	"gemini-3.5-flash-thinking-lite": {
		Mode:  5,
		Think: 0,
		Desc:  "Dynamic thinking with adaptive depth",
	},
	"gemini-flash-lite": {
		Mode:  6,
		Think: 4,
		Desc:  "Lightweight fast model",
	},
}

// ThinkSource records where the final thinking depth came from.
const (
	ThinkSourceModel  = "model"
	ThinkSourceSuffix = "suffix"
	ThinkSourceEffort = "effort"
)

// Thinking depth scale used by Gemini web (payload index 17).
// 0 is the deepest reasoning, 4 the shallowest.
const (
	ThinkDeepest    = 0
	ThinkMedium     = 2
	ThinkLow        = 3
	ThinkShallowest = 4
)

// effortToThink maps a reasoning effort label to Gemini web thinking depth.
var effortToThink = map[string]int{
	"none":    ThinkShallowest,
	"minimal": ThinkShallowest,
	"low":     ThinkLow,
	"medium":  ThinkMedium,
	"high":    ThinkDeepest,
	"xhigh":   ThinkDeepest,
	"max":     ThinkDeepest,
}

// effortAlias accepts common shorthands clients send.
var effortAlias = map[string]string{
	"min":     "minimal",
	"med":     "medium",
	"hi":      "high",
	"x-high":  "xhigh",
	"deep":    "high",
	"deepest": "xhigh",
}

// NormalizeEffort lowercases and expands shorthand aliases.
func NormalizeEffort(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if alias, ok := effortAlias[s]; ok {
		return alias
	}
	return s
}

// EffortToThink converts a reasoning effort label to a Gemini web thinking depth.
// The second return value is false when the label is empty or unrecognized.
func EffortToThink(raw string) (int, bool) {
	effort := NormalizeEffort(raw)
	if effort == "" {
		return 0, false
	}
	depth, ok := effortToThink[effort]
	return depth, ok
}

// SupportedEfforts lists every accepted reasoning effort label, for error messages.
func SupportedEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh"}
}

type Resolved struct {
	Name     string
	Mode     int
	Think    int
	Header   string
	Fallback bool
	Extra    map[int]any

	// ThinkSource is "suffix" when "@think=N" was parsed, "effort" when a
	// reasoning effort label decided the depth, and "model" for the default.
	ThinkSource string
	// Effort is the normalized reasoning effort label, empty when unspecified.
	Effort string
}

const DefaultModelName = "gemini-3.6-flash"

// Names returns every configured model id in sorted order. Output is
// deterministic so /v1/models responses are diffable and caches stable.
func Names() []string {
	names := make([]string, 0, len(MODELS))
	for k := range MODELS {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ResolveOptions carries per-request overrides that are not part of the model name.
type ResolveOptions struct {
	// ReasoningEffort accepts none/minimal/low/medium/high/xhigh.
	ReasoningEffort string
}

// Resolve maps a requested model string to its backend configuration using
// model defaults only. Kept for callers that have no per-request overrides.
func Resolve(modelName, defaultName string) (Resolved, error) {
	return ResolveWithOptions(modelName, defaultName, ResolveOptions{})
}

// ResolveWithOptions maps a requested model string to its backend configuration,
// applying per-request reasoning overrides.
//
// Precedence for thinking depth, highest first:
//  1. "@think=N" suffix in the model name (explicit and model-specific)
//  2. ReasoningEffort from the request (reasoning_effort / reasoning.effort /
//     generationConfig.thinkingConfig.thinkingLevel)
//  3. The model's default Think value
//
// An unrecognized effort label returns an error instead of silently using the
// model default, so client typos surface as HTTP 400.
func ResolveWithOptions(modelName, defaultName string, opts ResolveOptions) (Resolved, error) {
	if defaultName == "" {
		defaultName = DefaultModelName
	}

	var thinkOverride *int
	// The suffix must be the whole tail: "model@think=" and "a@think=2x" are
	// treated as plain model names rather than silently losing part of the id.
	if idx := strings.LastIndex(modelName, "@think="); idx != -1 && idx+len("@think=") < len(modelName) {
		thinkStr := modelName[idx+len("@think="):]
		modelName = modelName[:idx]
		val, err := strconv.Atoi(thinkStr)
		if err != nil {
			return Resolved{}, fmt.Errorf("invalid think level: %s", thinkStr)
		}
		if val < 0 || val > 4 {
			return Resolved{}, fmt.Errorf("think level must be between 0 (deepest) and 4 (shallowest), got %d", val)
		}
		thinkOverride = &val
	}

	effort := NormalizeEffort(opts.ReasoningEffort)
	var effortDepth *int
	if effort != "" {
		depth, ok := effortToThink[effort]
		if !ok {
			return Resolved{}, fmt.Errorf("invalid reasoning effort: %q (supported: %s)",
				opts.ReasoningEffort, strings.Join(SupportedEfforts(), ", "))
		}
		effortDepth = &depth
	}

	cfg, exists := MODELS[modelName]
	fallback := false
	if !exists {
		log.Printf("Unknown model '%s', falling back to '%s'", modelName, defaultName)
		modelName = defaultName
		cfg = MODELS[defaultName]
		fallback = true
	}

	thinkMode := cfg.Think
	thinkSource := ThinkSourceModel

	switch {
	case thinkOverride != nil:
		thinkMode = *thinkOverride
		thinkSource = ThinkSourceSuffix
	case effortDepth != nil:
		thinkMode = *effortDepth
		thinkSource = ThinkSourceEffort
	}

	return Resolved{
		Name:        modelName,
		Mode:        cfg.Mode,
		Think:       thinkMode,
		Header:      cfg.Header,
		Fallback:    fallback,
		Extra:       cfg.Extra,
		ThinkSource: thinkSource,
		Effort:      effort,
	}, nil
}
