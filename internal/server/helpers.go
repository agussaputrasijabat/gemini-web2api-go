package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/ikhsan3adi/gemini-web2api/internal/format"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
	"github.com/ikhsan3adi/gemini-web2api/internal/multimodal"
)

func setModelHeaders(w http.ResponseWriter, resolved models.Resolved) {
	w.Header().Set("X-Resolved-Model", resolved.Name)
	if resolved.Fallback {
		w.Header().Set("X-Model-Fallback", "true")
	}
	w.Header().Set("X-Resolved-Think", strconv.Itoa(resolved.Think))
	if resolved.ThinkSource != "" {
		w.Header().Set("X-Think-Source", resolved.ThinkSource)
	}
	if resolved.Effort != "" {
		w.Header().Set("X-Resolved-Effort", resolved.Effort)
	}
}

// requestEffort extracts a reasoning effort from an OpenAI chat request.
// reasoning_effort wins over the nested reasoning.effort form.
func requestEffort(req models.OpenAIChatRequest) string {
	if req.ReasoningEffort != "" {
		return req.ReasoningEffort
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		return req.Reasoning.Effort
	}
	return ""
}

// responsesEffort extracts a reasoning effort from a /v1/responses body.
// Accepts reasoning.effort and the flat reasoning_effort field.
func responsesEffort(req map[string]any) string {
	if r, ok := req["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok && e != "" {
			return e
		}
	}
	if e, ok := req["reasoning_effort"].(string); ok {
		return e
	}
	return ""
}

// googleEffort extracts a thinking level from a native Gemini request.
// thinkingLevel is preferred; the legacy thinkingBudget is converted to a
// coarse effort band so older clients still influence depth.
func googleEffort(req models.GoogleGenerateRequest) string {
	if req.GenerationConfig != nil && req.GenerationConfig.ThinkingConfig != nil {
		tc := req.GenerationConfig.ThinkingConfig
		if tc.ThinkingLevel != "" {
			return tc.ThinkingLevel
		}
		if tc.ThinkingBudget != nil {
			return budgetToEffort(*tc.ThinkingBudget)
		}
	}
	return ""
}

// toolOptions builds the format-layer options from configuration.
func (a *App) toolOptions() format.ToolOptions {
	budget := a.Cfg.ToolSchemaMaxBytes
	if budget <= 0 {
		budget = format.DefaultToolSchemaMaxBytes
	}
	return format.ToolOptions{MaxSchemaBytes: budget}
}

// budgetToEffort maps the legacy thinkingBudget integer onto an effort label.
// The native API treats 0 as "no thinking" and -1 as dynamic.
func budgetToEffort(b int) string {
	switch {
	case b < 0:
		return "high" // dynamic budget defaults to the deepest path
	case b == 0:
		return "none"
	case b <= 1024:
		return "low"
	case b <= 8192:
		return "medium"
	default:
		return "high"
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(data)
}

func marshalNoEscapeHTML(data any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func startSSE(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if ok {
		flusher.Flush()
	}
	return ok
}

func writeSSEData(w http.ResponseWriter, data any) error {
	enc, err := marshalNoEscapeHTML(data)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "data: %s\n\n", string(enc))
	if err != nil {
		return err
	}

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func writeSSEEvent(w http.ResponseWriter, event string, data any) error {
	enc, err := marshalNoEscapeHTML(data)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(enc))
	if err != nil {
		return err
	}

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func writeSSEDone(w http.ResponseWriter) error {
	_, err := fmt.Fprintf(w, "data: [DONE]\n\n")
	if err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (a *App) uploadImages(images []format.Image) ([]string, error) {
	if len(images) == 0 {
		return nil, nil
	}

	tokens := a.Tokens.Get()
	var fileRefs []string

	requester := a.Gem.HTTP
	if requester == nil {
		if a.HTTPClient != nil {
			requester = a.HTTPClient
		} else {
			requester = createHTTPClient(a.Cfg)
		}
	}

	for i, img := range images {
		data := img.Data

		// If image has URL but no data, fetch the bytes first.
		if len(data) == 0 && img.URL != "" {
			fetched, err := multimodal.FetchImageBytes(requester, img.URL)
			if err != nil {
				return nil, fmt.Errorf("image %d fetch failed: %w", i, err)
			}
			if len(fetched) == 0 {
				return nil, fmt.Errorf("image %d fetch returned empty data", i)
			}
			data = fetched
		}

		if len(data) == 0 {
			return nil, fmt.Errorf("image %d has no data", i)
		}

		// Detect MIME from magic bytes if not set or generic.
		mime := img.MIME
		if mime == "" || mime == "image/png" {
			mime = multimodal.DetectImageMime(data)
		}

		ref, err := multimodal.UploadImage(requester, tokens, data, mime, a.Gem.Cookies, a.Cfg.AuthUser)
		if err != nil {
			return nil, fmt.Errorf("image %d upload failed: %w", i, err)
		}
		if ref == "" {
			return nil, fmt.Errorf("image %d upload returned empty reference", i)
		}
		fileRefs = append(fileRefs, ref)
	}

	if len(fileRefs) == 0 {
		return nil, nil
	}
	return fileRefs, nil
}
