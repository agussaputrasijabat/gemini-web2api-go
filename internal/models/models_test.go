package models

import (
	"testing"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		input        string
		defaultName  string
		wantName     string
		wantMode     int
		wantThink    int
		wantHeader   string
		wantFallback bool
		wantErr      bool
	}{
		{
			input:        "gemini-3.6-flash",
			defaultName:  "gemini-3.6-flash",
			wantName:     "gemini-3.6-flash",
			wantMode:     1,
			wantThink:    4,
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "gemini-3.8-flash",
			defaultName:  "gemini-3.6-flash",
			wantName:     "gemini-3.8-flash",
			wantMode:     1,
			wantThink:    4,
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "gemini-4-argon",
			defaultName:  "gemini-3.6-flash",
			wantName:     "gemini-4-argon",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "argon",
			defaultName:  "gemini-3.6-flash",
			wantName:     "argon",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "gemini-4",
			defaultName:  "gemini-3.6-flash",
			wantName:     "gemini-4",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "argon@think=0",
			defaultName:  "gemini-3.6-flash",
			wantName:     "argon",
			wantMode:     8,
			wantThink:    0,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "gemini-3.5-flash-thinking",
			defaultName:  "gemini-3.6-flash",
			wantName:     "gemini-3.5-flash-thinking",
			wantMode:     2,
			wantThink:    0,
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "gemini-3.6-flash@think=0",
			defaultName:  "gemini-3.6-flash",
			wantName:     "gemini-3.6-flash",
			wantMode:     1,
			wantThink:    0,
			wantFallback: false,
			wantErr:      false,
		},
		{
			input:        "unknown-model",
			defaultName:  "gemini-3.6-flash",
			wantName:     "gemini-3.6-flash",
			wantMode:     1,
			wantThink:    4,
			wantFallback: true,
			wantErr:      false,
		},
		{
			input:       "gemini-3.6-flash@think=abc",
			defaultName: "gemini-3.6-flash",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			res, err := Resolve(tt.input, tt.defaultName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if res.Name != tt.wantName {
				t.Errorf("res.Name = %q, want %q", res.Name, tt.wantName)
			}
			if res.Mode != tt.wantMode {
				t.Errorf("res.Mode = %d, want %d", res.Mode, tt.wantMode)
			}
			if res.Think != tt.wantThink {
				t.Errorf("res.Think = %d, want %d", res.Think, tt.wantThink)
			}
			if res.Header != tt.wantHeader {
				t.Errorf("res.Header = %q, want %q", res.Header, tt.wantHeader)
			}
			if res.Fallback != tt.wantFallback {
				t.Errorf("res.Fallback = %v, want %v", res.Fallback, tt.wantFallback)
			}
		})
	}
}

func TestResolveEnhanced(t *testing.T) {
	res, err := Resolve("gemini-3.1-pro-enhanced", "gemini-3.6-flash")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if res.Extra == nil || res.Extra[31] != 2 || res.Extra[80] != 3 {
		t.Errorf("Expected extra fields {31:2, 80:3}, got %v", res.Extra)
	}
}

func TestResolveReasoningEffort(t *testing.T) {
	const def = "gemini-3.6-flash"

	tests := []struct {
		name       string
		model      string
		effort     string
		wantThink  int
		wantSource string
		wantEffort string
		wantErr    bool
	}{
		{"no effort uses model default", "gemini-3.8-flash", "", 4, ThinkSourceModel, "", false},
		{"high maps to deepest", "gemini-3.8-flash", "high", 0, ThinkSourceEffort, "high", false},
		{"medium maps to 2", "gemini-3.8-flash", "medium", 2, ThinkSourceEffort, "medium", false},
		{"low maps to 3", "gemini-3.8-flash", "low", 3, ThinkSourceEffort, "low", false},
		{"minimal maps to 4", "gemini-3.8-flash", "minimal", 4, ThinkSourceEffort, "minimal", false},
		{"none maps to 4", "gemini-3.8-flash", "none", 4, ThinkSourceEffort, "none", false},
		{"xhigh maps to 0", "gemini-3.8-flash", "xhigh", 0, ThinkSourceEffort, "xhigh", false},
		{"aliases are normalized", "gemini-3.8-flash", "HI", 0, ThinkSourceEffort, "high", false},
		{"alias min expands", "gemini-3.8-flash", "min", 4, ThinkSourceEffort, "minimal", false},
		{"whitespace trimmed", "gemini-3.8-flash", "  low  ", 3, ThinkSourceEffort, "low", false},
		// suffix wins over effort
		{"suffix beats effort", "gemini-3.8-flash@think=4", "high", 4, ThinkSourceSuffix, "high", false},
		// effort overrides the model default on thinking models too
		{"thinking model effort high", "gemini-3.5-flash-thinking", "high", 0, ThinkSourceEffort, "high", false},
		{"thinking model effort low", "gemini-3.5-flash-thinking", "low", 3, ThinkSourceEffort, "low", false},
		// invalid values must fail loudly
		{"bogus effort errors", "gemini-3.8-flash", "ultra", 0, "", "", true},
		{"think out of range", "gemini-3.8-flash@think=9", "", 0, "", "", true},
		{"think negative", "gemini-3.8-flash@think=-1", "", 0, "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := ResolveWithOptions(tt.model, def, ResolveOptions{ReasoningEffort: tt.effort})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if res.Think != tt.wantThink {
				t.Errorf("Think = %d, want %d", res.Think, tt.wantThink)
			}
			if res.ThinkSource != tt.wantSource {
				t.Errorf("ThinkSource = %q, want %q", res.ThinkSource, tt.wantSource)
			}
			if res.Effort != tt.wantEffort {
				t.Errorf("Effort = %q, want %q", res.Effort, tt.wantEffort)
			}
		})
	}
}

func TestEffortToThink(t *testing.T) {
	if _, ok := EffortToThink(""); ok {
		t.Error("empty effort should not resolve")
	}
	if _, ok := EffortToThink("nonsense"); ok {
		t.Error("unknown effort should not resolve")
	}
	if depth, ok := EffortToThink("High"); !ok || depth != ThinkDeepest {
		t.Errorf("High = %d/%v, want %d/true", depth, ok, ThinkDeepest)
	}
}
