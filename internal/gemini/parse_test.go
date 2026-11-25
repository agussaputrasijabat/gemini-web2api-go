package gemini

import (
	"testing"
)

func TestCleanText(t *testing.T) {
	input := "Hello world\n```python?code_reference&code_event_index=1\nprint('hi')```\nhttp://googleusercontent.com/card_content/123\nExtra text"
	cleaned := CleanText(input, true)
	expected := "Hello world\nExtra text"
	if cleaned != expected {
		t.Errorf("CleanText got %q, want %q", cleaned, expected)
	}
}

func TestIsBardError(t *testing.T) {
	raw := "something BardErrorInfo [1024] happened"
	code, ok := IsBardError(raw)
	if !ok || code != "1024" {
		t.Errorf("IsBardError got (%q, %t), want (\"1024\", true)", code, ok)
	}
}

func TestIsBardErrorCurrentFormat(t *testing.T) {
	raw := `BardErrorInfo",[1061,null,"Gemini isn't available for your account. (1061)"]`
	code, ok := IsBardError(raw)
	if !ok || code != "1061" {
		t.Fatalf("IsBardError got (%q, %t), want (\"1061\", true)", code, ok)
	}
	if msg := BardErrorMessage(raw); msg != "Gemini isn't available for your account. (1061)" {
		t.Errorf("BardErrorMessage = %q", msg)
	}
	if _, err := ExtractResponseText(raw); err == nil {
		t.Fatal("ExtractResponseText must report the upstream error")
	}
}
