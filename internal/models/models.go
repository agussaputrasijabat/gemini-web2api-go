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

// Upstream routing hex ids from Gemini's otAQ7b model catalog. The web app
// selects a model by sending this id in x-goog-ext-525001261-jspb; the legacy
// MODE_CATEGORY value still travels in payload index 79.
const (
	headerHydrogen  = "56fdd199312815e2" // 4 Hydrogen (same slot as the old 3.8 Flash)
	headerNeon      = "e6fa609c3fa255c0" // 4 Neon (took over 3.1 Pro's slot)
	headerCarbon    = "95a4c0abcba24364" // 4 Carbon (formerly 4 Argon)
	headerFlashLite = "cf41b0e0dd7d53e5" // 3.5 Flash-Lite / Auto
)

var MODELS = map[string]Model{
	// Current Gemini 4 web lineup.
	"gemini-4-hydrogen": {
		Mode:   1,
		Think:  4,
		Header: headerHydrogen,
		Desc:   "Gemini 4 Hydrogen — fast all-around model (needs a signed-in Google cookie)",
	},
	"gemini-4-hydrogen-thinking": {
		Mode:   1,
		Think:  0,
		Header: headerHydrogen,
		Desc:   "Gemini 4 Hydrogen with extended thinking",
		Extra:  map[int]any{80: 2},
	},
	"gemini-4-neon": {
		Mode:   3,
		Think:  4,
		Header: headerNeon,
		Desc:   "Gemini 4 Neon — frontier-level coding and analysis at lower cost (needs a paid/Ultra Google cookie)",
	},
	"gemini-4-neon-thinking": {
		Mode:   3,
		Think:  0,
		Header: headerNeon,
		Desc:   "Gemini 4 Neon with extended thinking",
		Extra:  map[int]any{80: 2},
	},
	"gemini-4-carbon": {
		Mode:   8,
		Think:  4,
		Header: headerCarbon,
		Desc:   "Gemini 4 Carbon — strongest model for complex tasks (needs a paid/Ultra Google cookie)",
		// inner[49]=40 is the browser parameter that keeps mode 8 from
		// hanging; upstream currently trades the thinking tier for it.
		Extra: map[int]any{49: 40},
	},
	"gemini-4-carbon-thinking": {
		Mode:   8,
		Think:  0,
		Header: headerCarbon,
		Desc:   "Gemini 4 Carbon with extended thinking (currently answers at the normal tier)",
		Extra:  map[int]any{49: 40, 80: 2},
	},
	"gemini-4-carbon-thinking-medium": {
		Mode:   8,
		Think:  0,
		Header: headerCarbon,
		Desc:   "Gemini 4 Carbon at the Medium thinking tier (no reasoning_content)",
		Extra:  map[int]any{49: 40, 80: 4},
	},

	// Element-name aliases kept for clients that adopted the first rename.
	"gemini-4-argon": {
		Mode:   8,
		Think:  4,
		Header: headerCarbon,
		Desc:   "Alias for gemini-4-carbon (upstream renamed 4 Argon to 4 Carbon)",
		Extra:  map[int]any{49: 40},
	},
	"gemini-4-argon-thinking": {
		Mode:   8,
		Think:  0,
		Header: headerCarbon,
		Desc:   "Alias for gemini-4-carbon-thinking",
		Extra:  map[int]any{49: 40, 80: 2},
	},
	"gemini-4-argon-thinking-medium": {
		Mode:   8,
		Think:  0,
		Header: headerCarbon,
		Desc:   "Alias for gemini-4-carbon-thinking-medium",
		Extra:  map[int]any{49: 40, 80: 4},
	},
	"argon": {
		Mode:   8,
		Think:  4,
		Header: headerCarbon,
		Desc:   "Alias for gemini-4-carbon",
		Extra:  map[int]any{49: 40},
	},
	"gemini-4": {
		Mode:   8,
		Think:  4,
		Header: headerCarbon,
		Desc:   "Alias for gemini-4-carbon",
		Extra:  map[int]any{49: 40},
	},

	// Legacy versioned names. Upstream upgraded these routes in place, so they
	// now resolve to the current Gemini 4 models instead of disappearing.
	"gemini-3.8-flash": {
		Mode:   1,
		Think:  4,
		Header: headerHydrogen,
		Desc:   "Legacy name for gemini-4-hydrogen",
	},
	"gemini-3.7-flash": {
		Mode:   1,
		Think:  4,
		Header: headerHydrogen,
		Desc:   "Legacy name for gemini-4-hydrogen",
	},
	"gemini-3.6-flash": {
		Mode:   1,
		Think:  4,
		Header: headerHydrogen,
		Desc:   "Legacy name for gemini-4-hydrogen (previous default)",
	},
	"gemini-3.6-flash-thinking": {
		Mode:   1,
		Think:  0,
		Header: headerHydrogen,
		Desc:   "Legacy name for gemini-4-hydrogen-thinking",
		Extra:  map[int]any{80: 2},
	},
	"gemini-3.5-flash": {
		Mode:   1,
		Think:  4,
		Header: headerHydrogen,
		Desc:   "Legacy name for gemini-4-hydrogen",
	},
	"gemini-3.5-flash-thinking": {
		Mode:   1,
		Think:  0,
		Header: headerHydrogen,
		Desc:   "Legacy name for gemini-4-hydrogen-thinking",
		Extra:  map[int]any{80: 2},
	},
	"gemini-3.5-flash-thinking-lite": {
		Mode:   6,
		Think:  0,
		Header: headerFlashLite,
		Desc:   "Legacy adaptive-thinking route (now 3.5 Flash-Lite)",
		Extra:  map[int]any{80: 2},
	},
	"gemini-3.1-pro": {
		Mode:   3,
		Think:  4,
		Header: headerNeon,
		Desc:   "Legacy name for gemini-4-neon",
	},
	"gemini-3.1-pro-thinking": {
		Mode:   3,
		Think:  0,
		Header: headerNeon,
		Desc:   "Legacy name for gemini-4-neon-thinking",
		Extra:  map[int]any{80: 2},
	},
	"gemini-3.1-pro-enhanced": {
		Mode:   3,
		Think:  0,
		Header: headerNeon,
		Desc:   "Legacy name for gemini-4-neon-thinking (experimental)",
		Extra:  map[int]any{80: 2},
	},
	"gemini-auto": {
		Mode:   6,
		Think:  4,
		Header: headerFlashLite,
		Desc:   "Auto model selection (currently 3.5 Flash-Lite)",
	},
	"gemini-3.5-flash-lite": {
		Mode:   6,
		Think:  4,
		Header: headerFlashLite,
		Desc:   "Alias for gemini-flash-lite",
	},
	"gemini-flash-lite": {
		Mode:   6,
		Think:  4,
		Header: headerFlashLite,
		Desc:   "Lightweight fast model (3.5 Flash-Lite)",
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

const DefaultModelName = "gemini-4-hydrogen"

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
