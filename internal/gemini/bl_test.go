package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

// --- test isolation ---------------------------------------------------------

// TestMain points the BL cache at a throwaway directory so tests never read or
// write the developer's real ~/Library/Caches entry.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gemini-bl-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv(BLCachePathEnv, filepath.Join(dir, "bl.json"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// --- fixtures ---------------------------------------------------------------

const (
	fixtureNewFormat = `<html><head><title>Gemini</title></head><body>
<script nonce="abc">window.WIZ_global_data = {
 "cfb2h": "boq_gemini-web-uiserver_20261106.06_p0",
 "FdrFJe": "some-other-value"};</script></body></html>`

	fixtureOldFormat = `<html><body><script>window.WIZ_global_data = {
 "cfb2h": "boq_assistant-bard-web-server_20260716.08_p0"};</script></body></html>`

	fixtureGenericFormat = `<html><body><script>var cfb2h="boq_some-unknown-server_20261201.03_p2";</script></body></html>`

	fixtureNoBL = `<html><body><h1>Sorry...</h1><p>unusual traffic</p></body></html>`

	fixtureBadDate = `<html><body><script>var bl="boq_gemini-web-uiserver_99999999.99_p0";</script></body></html>`
)

// --- requester stubs --------------------------------------------------------

// blStubRequester serves scripted responses keyed by request URL path/query and
// records every request it saw.
type blStubRequester struct {
	bodies   []string
	status   int
	location string
	seen     []*http.Request
	fail     error
}

func (s *blStubRequester) Do(req *http.Request) (*http.Response, error) {
	s.seen = append(s.seen, req)
	if s.fail != nil {
		return nil, s.fail
	}
	idx := len(s.seen) - 1
	body := ""
	if idx < len(s.bodies) {
		body = s.bodies[idx]
	} else if len(s.bodies) > 0 {
		body = s.bodies[len(s.bodies)-1]
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	h := make(http.Header)
	if s.location != "" {
		h.Set("Location", s.location)
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     h,
	}, nil
}

// --- BL detection -----------------------------------------------------------

func TestExtractBLFormats(t *testing.T) {
	tests := []struct {
		name        string
		html        string
		want        string
		wantPattern string
	}{
		{
			name:        "new gemini-web-uiserver format",
			html:        fixtureNewFormat,
			want:        "boq_gemini-web-uiserver_20261106.06_p0",
			wantPattern: "gemini-web-uiserver",
		},
		{
			name:        "legacy assistant-bard-web-server format",
			html:        fixtureOldFormat,
			want:        "boq_assistant-bard-web-server_20260716.08_p0",
			wantPattern: "assistant-bard-web-server",
		},
		{
			name:        "unknown server name falls through to generic pattern",
			html:        fixtureGenericFormat,
			want:        "boq_some-unknown-server_20261201.03_p2",
			wantPattern: "generic-boq",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, pattern, err := extractBL(tt.html)
			if err != nil {
				t.Fatalf("extractBL error: %v", err)
			}
			if got != tt.want {
				t.Errorf("extractBL = %q, want %q", got, tt.want)
			}
			if pattern != tt.wantPattern {
				t.Errorf("pattern = %q, want %q", pattern, tt.wantPattern)
			}
		})
	}
}

func TestExtractBLNewestFormatWins(t *testing.T) {
	// Both formats present: the newest server generation must win even when the
	// legacy label appears first in the document.
	html := `<script>a="boq_assistant-bard-web-server_20260716.08_p0";
	          b="boq_gemini-web-uiserver_20261106.06_p0";</script>`

	got, pattern, err := extractBL(html)
	if err != nil {
		t.Fatalf("extractBL error: %v", err)
	}
	if got != "boq_gemini-web-uiserver_20261106.06_p0" {
		t.Errorf("extractBL = %q, want newest format", got)
	}
	if pattern != "gemini-web-uiserver" {
		t.Errorf("pattern = %q, want gemini-web-uiserver", pattern)
	}
}

func TestExtractBLMissing(t *testing.T) {
	_, _, err := extractBL(fixtureNoBL)
	if err == nil {
		t.Fatal("extractBL on HTML without BL should fail")
	}
	if !strings.Contains(err.Error(), "no build label") {
		t.Errorf("error should explain the missing pattern, got: %v", err)
	}
}

func TestExtractBLRejectsInvalidDate(t *testing.T) {
	_, _, err := extractBL(fixtureBadDate)
	if err == nil {
		t.Fatal("extractBL should reject an unparseable YYYYMMDD date")
	}
	if !strings.Contains(err.Error(), "invalid date") {
		t.Errorf("error should mention invalid date, got: %v", err)
	}
}

func TestValidateBL(t *testing.T) {
	valid := []string{
		"boq_gemini-web-uiserver_20261106.06_p0",
		"boq_assistant-bard-web-server_20260716.08_p0",
		"boq_x_20200101.1_p9",
	}
	for _, bl := range valid {
		if err := validateBL(bl); err != nil {
			t.Errorf("validateBL(%q) = %v, want nil", bl, err)
		}
	}

	invalid := []string{
		"",
		"boq_",
		"gemini-web-uiserver_20261106.06_p0",
		"boq_gemini-web-uiserver_2026110.06_p0",  // 7-digit date
		"boq_gemini-web-uiserver_20261332.06_p0", // month 13
		"boq_gemini-web-uiserver_19990101.06_p0", // implausible year
		"boq_gemini-web-uiserver_20261106_p0",    // missing revision
	}
	for _, bl := range invalid {
		if err := validateBL(bl); err == nil {
			t.Errorf("validateBL(%q) = nil, want error", bl)
		}
	}
}

func TestFetchLatestBLNewFormat(t *testing.T) {
	stub := &blStubRequester{bodies: []string{fixtureNewFormat}}
	got, err := FetchLatestBL(stub)
	if err != nil {
		t.Fatalf("FetchLatestBL error: %v", err)
	}
	if got != "boq_gemini-web-uiserver_20261106.06_p0" {
		t.Errorf("FetchLatestBL = %q, want new format label", got)
	}
	if len(stub.seen) != 1 {
		t.Errorf("expected a single request, got %d", len(stub.seen))
	}
	if stub.seen[0].URL.Host != "gemini.google.com" {
		t.Errorf("FetchLatestBL did not use the provided Requester: %v", stub.seen[0].URL)
	}
	if _, ok := stub.seen[0].Context().Deadline(); !ok {
		t.Error("FetchLatestBL request should carry a timeout deadline")
	}
}

func TestFetchLatestBLNoBLReportsSources(t *testing.T) {
	stub := &blStubRequester{bodies: []string{fixtureNoBL}}
	_, err := FetchLatestBL(stub)
	if err == nil {
		t.Fatal("FetchLatestBL should fail when no page carries a BL")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BL auto-detect failed") {
		t.Errorf("error should be explicit about auto-detect failing, got: %v", err)
	}
	if !strings.Contains(msg, "/app") {
		t.Errorf("error should list the sources tried, got: %v", err)
	}
	if strings.Contains(msg, "pattern not found in response") {
		t.Errorf("error should no longer be the vague legacy message, got: %v", err)
	}
	// Every source must be attempted exactly once - no runaway loop. With no
	// cookie configured there are two anonymous sources (/app and /).
	if len(stub.seen) != 2 {
		t.Errorf("expected 2 anonymous source attempts, got %d", len(stub.seen))
	}
}

func TestFetchLatestBLWithCookieTriesAuthSource(t *testing.T) {
	stub := &blStubRequester{bodies: []string{fixtureNoBL}}
	_, err := FetchLatestBL(stub, BLFetchOptions{Cookie: "SID=abc", AuthUser: "0"})
	if err == nil {
		t.Fatal("expected failure when no source serves a BL")
	}
	if len(stub.seen) != 3 {
		t.Fatalf("expected 3 source attempts with cookie, got %d", len(stub.seen))
	}
	if got := stub.seen[1].Header.Get("Cookie"); got != "SID=abc" {
		t.Errorf("second attempt should send cookies, got Cookie=%q", got)
	}
	if got := stub.seen[0].Header.Get("Cookie"); got != "" {
		t.Errorf("first attempt should stay anonymous, got Cookie=%q", got)
	}
	if !strings.Contains(err.Error(), "authenticated") {
		t.Errorf("error should list the authenticated source as tried, got: %v", err)
	}
}

// mockStreamLine builds a bard stream frame that satisfies the shape and
// minimum-length checks in ExtractTextsFromLine.
func mockStreamLine(text string) string {
	inner := []any{nil, nil, nil, nil, []any{[]any{nil, []any{text}}}}
	for i := 0; i < 12; i++ {
		inner = append(inner, nil)
	}
	innerJSON, _ := json.Marshal(inner)

	outer := []any{[]any{"wrb.fr", nil, string(innerJSON)}}
	for i := 0; i < 40; i++ {
		outer = append(outer, nil)
	}
	outerJSON, _ := json.Marshal(outer)
	return "\n" + string(outerJSON) + "\n"
}

func TestFetchLatestBLFallbackToSecondSource(t *testing.T) {
	// First source lacks a BL, second one has it.
	stub := &blStubRequester{bodies: []string{fixtureNoBL, fixtureNewFormat}}
	got, err := FetchLatestBL(stub)
	if err != nil {
		t.Fatalf("FetchLatestBL error: %v", err)
	}
	if got != "boq_gemini-web-uiserver_20261106.06_p0" {
		t.Errorf("FetchLatestBL = %q, want fallback label", got)
	}
}

func TestFetchLatestBLBlockedRedirect(t *testing.T) {
	stub := &blStubRequester{
		status:   http.StatusFound,
		location: "https://www.google.com/sorry/index?continue=https://gemini.google.com/app",
	}
	_, err := FetchLatestBL(stub)
	if err == nil {
		t.Fatal("expected error for blocked redirect")
	}
	if !strings.Contains(err.Error(), "flagged") {
		t.Errorf("error should mention the IP being flagged, got: %v", err)
	}
}

// --- persistent cache -------------------------------------------------------

func TestBLCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "bl.json")

	if _, _, ok := LoadBLCache(path); ok {
		t.Fatal("empty cache path should not report a hit")
	}

	if err := SaveBLCache(path, "boq_gemini-web-uiserver_20261106.06_p0"); err != nil {
		t.Fatalf("SaveBLCache error: %v", err)
	}

	bl, fetchedAt, ok := LoadBLCache(path)
	if !ok {
		t.Fatal("LoadBLCache should hit after a save")
	}
	if bl != "boq_gemini-web-uiserver_20261106.06_p0" {
		t.Errorf("loaded BL = %q", bl)
	}
	if fetchedAt.IsZero() || time.Since(fetchedAt) > time.Hour {
		t.Errorf("unexpected fetched_at: %v", fetchedAt)
	}

	if err := SaveBLCache(path, "garbage"); err == nil {
		t.Error("SaveBLCache should reject an invalid label")
	}
}

func TestBLNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"boq_gemini-web-uiserver_20261106.06_p0", "boq_assistant-bard-web-server_20260716.08_p0", true},
		{"boq_gemini-web-uiserver_20261106.06_p0", "boq_gemini-web-uiserver_20261106.06_p0", false},
		{"boq_gemini-web-uiserver_20261106.05_p0", "boq_gemini-web-uiserver_20261106.06_p0", false},
		{"boq_gemini-web-uiserver_20261107.01_p0", "boq_gemini-web-uiserver_20261106.99_p0", true},
		{"garbage", "boq_gemini-web-uiserver_20261106.06_p0", false},
		{"boq_gemini-web-uiserver_20261106.06_p0", "garbage", true},
	}
	for _, tt := range cases {
		if got := BLNewer(tt.a, tt.b); got != tt.want {
			t.Errorf("BLNewer(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAdoptCachedBLPrefersNewerCache(t *testing.T) {
	c := &Client{
		Cfg:     config.Config{GeminiBL: "boq_assistant-bard-web-server_20260716.08_p0"},
		bl:      "boq_assistant-bard-web-server_20260716.08_p0",
		blCache: filepath.Join(t.TempDir(), "bl.json"),
	}
	if err := SaveBLCache(c.blCache, "boq_gemini-web-uiserver_20261106.06_p0"); err != nil {
		t.Fatalf("SaveBLCache: %v", err)
	}

	adopted, previous, changed := c.AdoptCachedBL()
	if !changed {
		t.Fatal("AdoptCachedBL should adopt a newer cached label")
	}
	if adopted != "boq_gemini-web-uiserver_20261106.06_p0" || previous != "boq_assistant-bard-web-server_20260716.08_p0" {
		t.Errorf("AdoptCachedBL = (%q, %q)", adopted, previous)
	}
	if got := c.CurrentBL(); got != adopted {
		t.Errorf("CurrentBL() = %q, want adopted %q", got, adopted)
	}
}

func TestAdoptCachedBLKeepsNewerConfig(t *testing.T) {
	c := &Client{
		Cfg:     config.Config{GeminiBL: "boq_gemini-web-uiserver_20261107.01_p0"},
		bl:      "boq_gemini-web-uiserver_20261107.01_p0",
		blCache: filepath.Join(t.TempDir(), "bl.json"),
	}
	if err := SaveBLCache(c.blCache, "boq_gemini-web-uiserver_20261106.06_p0"); err != nil {
		t.Fatalf("SaveBLCache: %v", err)
	}

	if _, _, changed := c.AdoptCachedBL(); changed {
		t.Error("AdoptCachedBL must not downgrade to an older cached label")
	}
	if got := c.CurrentBL(); got != "boq_gemini-web-uiserver_20261107.01_p0" {
		t.Errorf("CurrentBL() = %q, want config value preserved", got)
	}
}

// --- 405 handling -----------------------------------------------------------

// scriptedRequester returns queued responses in order and counts calls.
type scriptedRequester struct {
	responses []*scriptedResp
	calls     int
}

func (s *scriptedRequester) Do(req *http.Request) (*http.Response, error) {
	s.calls++
	idx := s.calls - 1
	if idx >= len(s.responses) {
		idx = len(s.responses) - 1
	}
	src := s.responses[idx]
	return &http.Response{
		StatusCode: src.StatusCode,
		Body:       io.NopCloser(strings.NewReader(src.bodyString)),
		Header:     src.Header,
	}, nil
}

// scriptedResp declares a response without needing a live body reader.
type scriptedResp struct {
	StatusCode int
	bodyString string
	Header     http.Header
}

func mkResp(status int, body string, location string) *scriptedResp {
	h := make(http.Header)
	if location != "" {
		h.Set("Location", location)
	}
	return &scriptedResp{StatusCode: status, bodyString: body, Header: h}
}

// asHTTPResp converts a scriptedResp into a real *http.Response so triageStatus
// can be exercised directly.
func asHTTPResp(r *scriptedResp) *http.Response {
	return &http.Response{
		StatusCode: r.StatusCode,
		Body:       io.NopCloser(strings.NewReader(r.bodyString)),
		Header:     r.Header,
	}
}

func newTestClient(t *testing.T, req Requester, attempts int) *Client {
	t.Helper()
	t.Setenv(BLCachePathEnv, filepath.Join(t.TempDir(), "bl.json"))
	c := &Client{
		Cfg: config.Config{
			GeminiBL:      "boq_assistant-bard-web-server_20260716.08_p0",
			RetryAttempts: attempts,
			RetryDelaySec: 0,
		},
		HTTP:    req,
		Cookies: NewCookieCache(""),
		Logf:    func(string, ...any) {},
		bl:      "boq_assistant-bard-web-server_20260716.08_p0",
		blCache: filepath.Join(t.TempDir(), "bl.json"),
	}
	return c
}

func TestGenerate405WithFailedRefreshDoesNotRetry(t *testing.T) {
	// Every request 405s. The BL scrape also fails (no BL in HTML), so no
	// retry may happen after the failed refresh.
	req := &scriptedRequester{responses: []*scriptedResp{
		mkResp(http.StatusMethodNotAllowed, fixtureNoBL, ""),
	}}
	c := newTestClient(t, req, 3)
	// Force the BL fetch to see only HTML without a label and to keep using the
	// same HTTP stub for both chat and scrape bodies.
	c.Cfg.GeminiBL = "boq_assistant-bard-web-server_20260716.08_p0"

	_, err := c.Generate(context.Background(), "hi", ModelOptions{ModelID: 1}, nil)
	if err == nil {
		t.Fatal("Generate should fail on persistent 405")
	}
	if !isBLError(err) {
		t.Errorf("error kind should be bl_expired, got: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "405") {
		t.Errorf("error should mention HTTP 405, got: %v", err)
	}
	if !strings.Contains(msg, "boq_assistant-bard-web-server_20260716.08_p0") {
		t.Errorf("error should name the BL in use, got: %v", msg)
	}
	if !strings.Contains(msg, "BL refresh failed") {
		t.Errorf("error should state the refresh failed, got: %v", msg)
	}
	// One chat request + one scrape attempt per source; never RetryAttempts
	// chat requests with the same stale BL.
	if req.calls > 4 {
		t.Errorf("expected at most 4 upstream calls (1 chat + 3 scrape sources), got %d", req.calls)
	}
}

func TestGenerate405RefreshSuccessRetriesOnce(t *testing.T) {
	newBL := "boq_gemini-web-uiserver_20261106.06_p0"

	// Call 1: chat -> 405. Calls 2..4: BL scrape sources; first scrape hits.
	// Call 5: chat retried with the refreshed BL -> success payload.
	successBody := mockStreamLine("ok")
	req := &scriptedRequester{responses: []*scriptedResp{
		mkResp(http.StatusMethodNotAllowed, "", ""),
		mkResp(http.StatusOK, `<script>cfb2h="`+newBL+`"</script>`, ""),
		mkResp(http.StatusOK, successBody, ""),
	}}
	c := newTestClient(t, req, 3)

	text, err := c.Generate(context.Background(), "hi", ModelOptions{ModelID: 1}, nil)
	if err != nil {
		t.Fatalf("Generate after BL refresh should succeed, got: %v", err)
	}
	if text != "ok" {
		t.Errorf("Generate text = %q, want \"ok\"", text)
	}
	if got := c.CurrentBL(); got != newBL {
		t.Errorf("CurrentBL() = %q, want refreshed %q", got, newBL)
	}
	if req.calls != 3 {
		t.Errorf("expected 3 upstream calls (405, BL scrape, retry), got %d", req.calls)
	}
}

func TestGenerateStream405WithFailedRefreshDoesNotRetry(t *testing.T) {
	req := &scriptedRequester{responses: []*scriptedResp{
		mkResp(http.StatusMethodNotAllowed, fixtureNoBL, ""),
	}}
	c := newTestClient(t, req, 3)

	err := c.GenerateStream(context.Background(), "hi", ModelOptions{ModelID: 1}, nil, func(string) error { return nil })
	if err == nil {
		t.Fatal("GenerateStream should fail on persistent 405")
	}
	if !isBLError(err) {
		t.Errorf("error kind should be bl_expired, got: %v", err)
	}
	if !strings.Contains(err.Error(), "BL refresh failed") {
		t.Errorf("error should state the refresh failed, got: %v", err)
	}
	if req.calls > 4 {
		t.Errorf("expected at most 4 upstream calls, got %d", req.calls)
	}
}

// --- blocked IP handling ----------------------------------------------------

func TestTriageStatusClassifiesBlocked(t *testing.T) {
	c := newTestClient(t, &scriptedRequester{responses: []*scriptedResp{mkResp(200, "", "")}}, 3)

	resp := mkResp(http.StatusFound, "", "https://www.google.com/sorry/index?continue=https://gemini.google.com/app")
	err := c.triageStatus(asHTTPResp(resp))
	if err == nil {
		t.Fatal("sorry/index redirect should be an error")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UpstreamError, got %T", err)
	}
	if ue.Kind != KindBlocked {
		t.Errorf("Kind = %q, want %q", ue.Kind, KindBlocked)
	}
	if !strings.Contains(ue.Msg, "sorry/index") || !strings.Contains(ue.Msg, "flagged") {
		t.Errorf("message should explain the block, got: %q", ue.Msg)
	}
	if retryable(err) {
		t.Error("blocked must not be retryable")
	}
}

func TestTriageStatusClassifies405(t *testing.T) {
	c := newTestClient(t, &scriptedRequester{responses: []*scriptedResp{mkResp(200, "", "")}}, 3)
	err := c.triageStatus(asHTTPResp(mkResp(http.StatusMethodNotAllowed, "", "")))
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Kind != KindBLEpired {
		t.Fatalf("405 should map to KindBLEpired, got %v", err)
	}
	if retryable(err) {
		t.Error("bl_expired must not be blindly retryable")
	}
}

func TestGenerateBlockedDoesNotRetry(t *testing.T) {
	req := &scriptedRequester{responses: []*scriptedResp{
		mkResp(http.StatusFound, "", "https://www.google.com/sorry/index"),
	}}
	c := newTestClient(t, req, 5)

	_, err := c.Generate(context.Background(), "hi", ModelOptions{ModelID: 1}, nil)
	if err == nil {
		t.Fatal("Generate should surface the block")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Kind != KindBlocked {
		t.Fatalf("expected blocked error, got: %v", err)
	}
	if !strings.Contains(ue.Msg, "ganti proxy") {
		t.Errorf("message should advise a proxy change, got: %q", ue.Msg)
	}
	if req.calls != 1 {
		t.Errorf("blocked IP must not be retried: got %d upstream calls, want 1", req.calls)
	}
}

func TestGenerateStreamBlockedDoesNotRetry(t *testing.T) {
	req := &scriptedRequester{responses: []*scriptedResp{
		mkResp(http.StatusFound, "", "https://www.google.com/sorry/index"),
	}}
	c := newTestClient(t, req, 5)

	err := c.GenerateStream(context.Background(), "hi", ModelOptions{ModelID: 1}, nil, func(string) error { return nil })
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Kind != KindBlocked {
		t.Fatalf("expected blocked error, got: %v", err)
	}
	if req.calls != 1 {
		t.Errorf("stream blocked IP must not be retried: got %d upstream calls, want 1", req.calls)
	}
}

// --- BL accessors -----------------------------------------------------------

func TestCurrentBL(t *testing.T) {
	c := &Client{
		bl: "mutable-bl",
		Cfg: config.Config{
			GeminiBL: "config-bl",
		},
	}
	if got := c.CurrentBL(); got != "mutable-bl" {
		t.Errorf("CurrentBL() = %q, want %q", got, "mutable-bl")
	}

	c2 := &Client{
		Cfg: config.Config{
			GeminiBL: "config-bl",
		},
	}
	if got := c2.CurrentBL(); got != "config-bl" {
		t.Errorf("CurrentBL() empty bl = %q, want %q", got, "config-bl")
	}
}

func TestSetBL(t *testing.T) {
	c := &Client{
		Cfg: config.Config{
			GeminiBL: "old",
		},
	}
	c.SetBL("new")
	if got := c.CurrentBL(); got != "new" {
		t.Errorf("After SetBL, CurrentBL() = %q, want %q", got, "new")
	}
}
