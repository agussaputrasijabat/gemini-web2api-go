package gemini

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	reCodeExec    = regexp.MustCompile(`(?s)\x60\x60\x60(?:python|javascript|text)\?code_(?:reference|stdout)&code_event_index=\d+\n.*?\x60\x60\x60\n?`)
	reCardContent = regexp.MustCompile(`http://googleusercontent\.com/card_content/\d+\n?`)
	// BardErrorInfo appears both as the legacy `BardErrorInfo [1024]` text and
	// as the current protobuf-like JSON: BardErrorInfo",[1061,null,"message"].
	reBardError    = regexp.MustCompile(`BardErrorInfo(?:"\s*,\s*\[|\s*\[)\s*(\d+)`)
	reBardErrorMsg = regexp.MustCompile(`BardErrorInfo"\s*,\s*\[\s*\d+\s*,\s*null\s*,\s*"([^"]*)"`)
)

func CleanText(text string, strip bool) string {
	text = reCodeExec.ReplaceAllString(text, "")
	text = reCardContent.ReplaceAllString(text, "")
	if strip {
		return strings.TrimSpace(text)
	}
	return text
}

func ExtractTextsFromLine(line string) []string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
		return nil
	}

	var arr []any
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return nil
	}

	firstElem, ok := arr[0].([]any)
	if !ok || len(firstElem) < 3 {
		return nil
	}

	innerStr, ok := firstElem[2].(string)
	if !ok || len(innerStr) < 50 {
		return nil
	}

	var inner []any
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
		return nil
	}

	if len(inner) <= 4 || inner[4] == nil {
		return nil
	}

	parts, ok := inner[4].([]any)
	if !ok {
		return nil
	}

	var texts []string
	for _, part := range parts {
		partList, ok := part.([]any)
		if ok && len(partList) > 1 && partList[1] != nil {
			tList, ok := partList[1].([]any)
			if ok {
				for _, t := range tList {
					if str, ok := t.(string); ok && str != "" {
						texts = append(texts, str)
					}
				}
			}
		}
	}

	return texts
}

func IsBardError(raw string) (string, bool) {
	match := reBardError.FindStringSubmatch(raw)
	if len(match) > 1 {
		return match[1], true
	}
	return "", false
}

// BardErrorMessage returns the human-readable message that accompanies a
// BardErrorInfo frame, when upstream provides one.
func BardErrorMessage(raw string) string {
	match := reBardErrorMsg.FindStringSubmatch(raw)
	if len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func ExtractResponseText(raw string) (string, error) {
	if code, ok := IsBardError(raw); ok {
		if msg := BardErrorMessage(raw); msg != "" {
			return "", fmt.Errorf("gemini upstream rejected request (BardErrorInfo %s): %s", code, msg)
		}
		return "", fmt.Errorf("gemini upstream rejected request: BardErrorInfo [%s]", code)
	}

	var lastText string
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		texts := ExtractTextsFromLine(line)
		for _, t := range texts {
			if len(t) > len(lastText) {
				lastText = t
			}
		}
	}

	return CleanText(lastText, true), nil
}
