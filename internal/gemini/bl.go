package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	blBaseURL = "https://gemini.google.com"
	blAppPath = "/app"

	// blFetchTimeout bounds the whole multi-source detection attempt so startup
	// never hangs on a slow or blocked upstream.
	blFetchTimeout = 20 * time.Second
	// blSourceTimeout bounds a single source request. Worst case the caller
	// waits blFetchTimeout, not n*blSourceTimeout, because the sources share
	// the parent context deadline.
	blSourceTimeout = 8 * time.Second
	// blMaxBody caps how much HTML is read per source. The real /app page is
	// far below this; the cap only protects against pathological responses.
	blMaxBody = 2 << 20
)

// blPattern is one candidate build-label shape. Order matters: the newest
// server generation is tried first and the generic pattern is the last resort.
// Adding a new Google format means prepending one entry here, not editing the
// extraction logic.
type blPattern struct {
	name string
	re   *regexp.Regexp
}

var blPatterns = []blPattern{
	{"gemini-web-uiserver", regexp.MustCompile(`boq_gemini-web-uiserver_\d{8}\.\d+_p\d+`)},
	{"assistant-bard-web-server", regexp.MustCompile(`boq_assistant-bard-web-server_\d{8}\.\d+_p\d+`)},
	{"generic-boq", regexp.MustCompile(`boq_[a-z0-9-]+_\d{8}\.\d+_p\d+`)},
}

// reBLStamp captures the date/revision/patch parts shared by every known
// build-label format: boq_<server>_YYYYMMDD.<rev>_p<PATCH>.
var reBLStamp = regexp.MustCompile(`_(\d{8})\.(\d+)_p(\d+)$`)

// validateBL rejects anything that is not a plausible build label. This keeps
// random "boq_*" tokens from unparsed HTML out of the request URL.
func validateBL(bl string) error {
	if bl == "" {
		return errors.New("empty build label")
	}
	if len(bl) < 16 || len(bl) > 96 {
		return fmt.Errorf("build label %q has implausible length %d", bl, len(bl))
	}
	if !strings.HasPrefix(bl, "boq_") {
		return fmt.Errorf("build label %q does not start with boq_", bl)
	}
	m := reBLStamp.FindStringSubmatch(bl)
	if m == nil {
		return fmt.Errorf("build label %q does not match boq_<server>_YYYYMMDD.<rev>_p<N>", bl)
	}
	date, err := time.Parse("20060102", m[1])
	if err != nil {
		return fmt.Errorf("build label %q has invalid date %q", bl, m[1])
	}
	if date.Year() < 2020 || date.After(time.Now().AddDate(1, 0, 0)) {
		return fmt.Errorf("build label %q has implausible date %q", bl, m[1])
	}
	return nil
}

// extractBL scans HTML with every known pattern in priority order and returns
// the first candidate that validates, together with the pattern that matched.
func extractBL(html string) (string, string, error) {
	var invalid []string
	for _, p := range blPatterns {
		for _, match := range p.re.FindAllString(html, -1) {
			if err := validateBL(match); err != nil {
				invalid = append(invalid, err.Error())
				continue
			}
			return match, p.name, nil
		}
	}
	if len(invalid) > 0 {
		return "", "", fmt.Errorf("build label candidates rejected: %s", strings.Join(invalid, "; "))
	}
	return "", "", errors.New("no build label (boq_<server>_YYYYMMDD.<rev>_p<N>) found in page")
}

// BLFetchOptions carries optional auth context used by the fallback source.
// The /app page can render differently for a signed-in session, so a failed
// anonymous scrape retries once with the configured cookies.
type BLFetchOptions struct {
	AuthUser string
	Cookie   string
	SAPISID  string
	Logf     func(format string, args ...any)
	// Profile is the impersonation profile name in effect for the requester. It
	// only selects the matching browser header set; the transport itself is the
	// caller's. Empty means "no impersonation" (stdlib path).
	Profile string
}

type blSource struct {
	name     string
	url      string
	withAuth bool
}

func (o BLFetchOptions) sources() []blSource {
	prefix := AccountPrefix(o.AuthUser)
	srcs := []blSource{{
		name: "anonymous " + prefix + blAppPath,
		url:  blBaseURL + prefix + blAppPath,
	}}
	if o.Cookie != "" {
		srcs = append(srcs, blSource{
			name:     "authenticated " + prefix + blAppPath,
			url:      blBaseURL + prefix + blAppPath,
			withAuth: true,
		})
	}
	srcs = append(srcs, blSource{name: "anonymous /", url: blBaseURL + "/"})
	return srcs
}

func (o BLFetchOptions) headers(withAuth bool) http.Header {
	// /app is a top-level document navigation, so it gets the document header
	// dialect (HTML Accept, navigate Sec-Fetch-*) for the active profile. Using
	// the same builder as chat requests keeps a single source of truth for UA
	// and encoding, so the BL scrape cannot send a UA that disagrees with the
	// TLS fingerprint.
	h := BrowserHeaders(o.Profile, true)
	if !withAuth {
		return h
	}
	if o.AuthUser != "" {
		h.Set("X-Goog-AuthUser", o.AuthUser)
	}
	if o.Cookie != "" {
		h.Set("Cookie", o.Cookie)
	}
	if o.SAPISID != "" {
		if hash := SAPISIDHash(o.SAPISID); hash != "" {
			h.Set("Authorization", hash)
		}
	}
	return h
}

// errBLBlocked signals Google's abuse interstitial. It aborts the multi-source
// BL lookup immediately: extra requests from a flagged IP only deepen the block.
var errBLBlocked = errors.New("blocked by Google abuse interstitial")

// FetchLatestBL fetches the latest BL identifier from the Gemini frontend.
// It issues GETs through the provided Requester so proxy/TLS configuration is
// honored, tries several pages/patterns, and returns a validated label.
//
// opts is optional and backward compatible with FetchLatestBL(client).
func FetchLatestBL(client Requester, opts ...BLFetchOptions) (string, error) {
	if client == nil {
		return "", errors.New("BL fetch: nil HTTP requester")
	}
	var o BLFetchOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	ctx, cancel := context.WithTimeout(context.Background(), blFetchTimeout)
	defer cancel()

	var tried []string
	var lastErr error
	for _, src := range o.sources() {
		bl, pattern, err := fetchBLFrom(ctx, client, src, o)
		if err == nil {
			logf("BL: got %s from %q (pattern %s)", bl, src.name, pattern)
			return bl, nil
		}
		lastErr = err
		tried = append(tried, fmt.Sprintf("%s: %v", src.name, err))
		logf("BL: source %q failed: %v", src.name, err)
		if errors.Is(err, errBLBlocked) {
			// Do not probe further sources: this IP is flagged.
			break
		}
	}

	if lastErr == nil {
		lastErr = errors.New("no source available")
	}
	return "", fmt.Errorf("BL auto-detect failed; tried %d source(s) [%s]: %w",
		len(tried), strings.Join(tried, " | "), lastErr)
}

func fetchBLFrom(ctx context.Context, client Requester, src blSource, o BLFetchOptions) (string, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, blSourceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, src.url, nil)
	if err != nil {
		return "", "", fmt.Errorf("request build failed: %w", err)
	}
	req.Header = o.headers(src.withAuth)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, blMaxBody))
	if err != nil {
		return "", "", fmt.Errorf("read failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		loc := resp.Header.Get("Location")
		if isSorryLocation(loc) {
			return "", "", fmt.Errorf("%w: HTTP %d redirect to %s (IP flagged by Google)", errBLBlocked, resp.StatusCode, loc)
		}
		if isRedirectStatus(resp.StatusCode) {
			return "", "", fmt.Errorf("HTTP %d redirect to %s", resp.StatusCode, locationOrUnknown(loc))
		}
		return "", "", fmt.Errorf("unexpected HTTP %d", resp.StatusCode)
	}

	bl, pattern, err := extractBL(string(body))
	if err != nil {
		return "", "", fmt.Errorf("%w (after reading %d bytes)", err, len(body))
	}
	return bl, pattern, nil
}

func locationOrUnknown(loc string) string {
	if loc == "" {
		return "an unknown location (no Location header)"
	}
	return loc
}

// isRedirectStatus reports whether code is a 3xx the client must interpret
// itself (redirects are never followed: they carry the block signal).
func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// isSorryLocation reports whether a redirect Location points at Google's
// abuse/block interstitial.
func isSorryLocation(loc string) bool {
	return loc != "" && strings.Contains(strings.ToLower(loc), "sorry/index")
}

// --- persistent cache -------------------------------------------------------

type blCacheEntry struct {
	BL        string    `json:"bl"`
	FetchedAt time.Time `json:"fetched_at"`
}

// BLCachePathEnv overrides the default cache location. Setting it to an empty
// value, "off", "none" or "disabled" disables persistence (useful in tests).
const BLCachePathEnv = "GEMINI_BL_CACHE"

// defaultBLCachePath returns the on-disk BL cache path, or "" when persistence
// is disabled. The file lives in the OS cache dir, never in the repo.
func defaultBLCachePath() string {
	if raw, ok := os.LookupEnv(BLCachePathEnv); ok {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "", "off", "none", "disabled":
			return ""
		}
		return raw
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "gemini-web2api", "bl.json")
}

// LoadBLCache reads a previously persisted, validated build label.
func LoadBLCache(path string) (string, time.Time, bool) {
	if path == "" {
		return "", time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, false
	}
	var entry blCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", time.Time{}, false
	}
	if validateBL(entry.BL) != nil {
		return "", time.Time{}, false
	}
	return entry.BL, entry.FetchedAt, true
}

// SaveBLCache atomically persists a validated build label.
func SaveBLCache(path, bl string) error {
	if path == "" {
		return nil
	}
	if err := validateBL(bl); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(blCacheEntry{BL: bl, FetchedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func blVersionParts(bl string) (date, rev, patch int, ok bool) {
	m := reBLStamp.FindStringSubmatch(bl)
	if m == nil {
		return 0, 0, 0, false
	}
	d, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, 0, false
	}
	r, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, 0, 0, false
	}
	p, err := strconv.Atoi(m[3])
	if err != nil {
		return 0, 0, 0, false
	}
	return d, r, p, true
}

// BLNewer reports whether a is a newer build label than b. Unknown/unparseable
// labels never win, so a corrupt cache cannot downgrade a valid config value.
func BLNewer(a, b string) bool {
	ad, ar, ap, aok := blVersionParts(a)
	if !aok {
		return false
	}
	bd, br, bp, bok := blVersionParts(b)
	if !bok {
		return true
	}
	switch {
	case ad != bd:
		return ad > bd
	case ar != br:
		return ar > br
	default:
		return ap > bp
	}
}
