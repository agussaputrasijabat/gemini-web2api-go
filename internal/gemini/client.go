package gemini

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

type UpstreamError struct {
	Status int
	Kind   string
	Msg    string
}

func (e *UpstreamError) Error() string {
	return e.Msg
}

// Upstream error kinds. They drive retry decisions: only transient kinds are
// worth another request.
const (
	// KindBlocked means Google redirected us to the abuse interstitial
	// (sorry/index). Retrying from the same IP only extends the block.
	KindBlocked = "blocked"
	// KindBLEpired means the build label in the query string is stale (HTTP 405).
	KindBLEpired = "bl_expired"
	// KindAuth covers 401/403 and account-selection redirects.
	KindAuth = "auth"
	// KindRateLimited is HTTP 429 from Google's edge.
	KindRateLimited = "rate_limited"
	// KindHTTP is any other unexpected HTTP status.
	KindHTTP = "http"
	// KindTransport is a network/TLS/timeout failure.
	KindTransport = "transport"
	// KindBard is a rejected/empty Gemini payload.
	KindBard = "bard"
)

type Requester interface {
	Do(req *http.Request) (*http.Response, error)
}

type ModelOptions struct {
	ModelName   string
	ModelID     int
	ThinkMode   int
	ModelHeader string
	Extra       map[int]any
}

type Client struct {
	Cfg     config.Config
	HTTP    Requester
	Cookies *CookieCache
	Logf    func(format string, args ...any)
	blMu    sync.Mutex
	bl      string
	blCache string // path to the persistent BL cache; "" disables it

	// blFetchMu serializes BL refreshes so a burst of 405s produces one scrape
	// instead of one scrape per in-flight request.
	blFetchMu sync.Mutex

	modelsMu      sync.RWMutex
	discovered    []DiscoveredModel
	accountCap    int
	accountCapFld int
	discMu        sync.Mutex
	lastDiscFail  time.Time
}

func NewClient(cfg config.Config) *Client {
	var req Requester
	// Impersonation and proxy are independent: the combination is the one that
	// actually helps (clean IP + browser TLS fingerprint), so a configured proxy
	// must not disable impersonation. getTLSClient passes the proxy down to
	// tls-client's own dialer.
	if cfg.Impersonate != "" {
		tlsAdapter, err := getTLSClient(cfg.Impersonate, cfg.RequestTimeoutSec, cfg.Proxy)
		if err != nil {
			log.Printf("Failed to create TLS client for profile %s: %v, falling back to stdlib", cfg.Impersonate, err)
		} else {
			req = tlsAdapter
		}
	}

	if req == nil {
		transport := &http.Transport{
			DisableCompression: true,
		}

		if cfg.Proxy != "" {
			if proxyURL, err := url.Parse(cfg.Proxy); err == nil {
				transport.Proxy = http.ProxyURL(proxyURL)
			}
		} else {
			transport.Proxy = http.ProxyFromEnvironment
		}

		req = &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.RequestTimeoutSec) * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	logFn := func(format string, args ...any) {
		if cfg.LogRequests {
			log.Printf(format, args...)
		}
	}

	client := &Client{
		Cfg:     cfg,
		HTTP:    req,
		Cookies: NewCookieCache(cfg.CookieFile),
		Logf:    logFn,
		bl:      cfg.GeminiBL,
		blCache: defaultBLCachePath(),
	}
	client.adoptCacheOnInit()
	return client
}

// adoptCacheOnInit promotes a newer, validated BL from the persistent cache.
// It runs once when the client is built, so a good label survives restarts
// even when the upstream is blocked during boot.
func (c *Client) adoptCacheOnInit() {
	adopted, previous, changed := c.AdoptCachedBL()
	if changed && c.Logf != nil {
		c.Logf("BL adopted from disk cache: %s -> %s", previous, adopted)
	}
}

// CurrentBL returns the current BL value, preferring the mutable field over config.
func (c *Client) CurrentBL() string {
	c.blMu.Lock()
	defer c.blMu.Unlock()
	if c.bl != "" {
		return c.bl
	}
	return c.Cfg.GeminiBL
}

// SetBL updates the BL value.
func (c *Client) SetBL(bl string) {
	c.blMu.Lock()
	defer c.blMu.Unlock()
	c.bl = bl
}

// UpdateBLIfNeeded fetches the latest BL from Gemini and updates it if different from current.
// Returns (newBL, changed, error).
func (c *Client) UpdateBLIfNeeded() (string, bool, error) {
	newBL, err := c.fetchBL()
	if err != nil {
		return "", false, err
	}
	if newBL == "" {
		return "", false, fmt.Errorf("empty BL fetched")
	}
	oldBL := c.CurrentBL()
	if newBL == oldBL {
		return newBL, false, nil
	}
	c.SetBL(newBL)
	c.Logf("BL updated: %s -> %s", oldBL, newBL)
	return newBL, true, nil
}

// fetchBL calls FetchLatestBL with the current session's auth context, records
// the outcome, and persists a successful result to the on-disk cache.
func (c *Client) fetchBL() (string, error) {
	opts := BLFetchOptions{Logf: c.Logf, Profile: c.profile()}
	if c.Cookies != nil {
		if info, err := c.Cookies.Load(); err == nil {
			opts.Cookie = info.Cookie
			opts.SAPISID = info.SAPISID
			opts.AuthUser = c.Cfg.AuthUser
			if opts.AuthUser == "" {
				opts.AuthUser = info.AuthUser
			}
		}
	}
	if opts.AuthUser == "" {
		opts.AuthUser = c.Cfg.AuthUser
	}

	c.blFetchMu.Lock()
	defer c.blFetchMu.Unlock()

	bl, err := FetchLatestBL(c.HTTP, opts)
	if err != nil {
		return "", err
	}
	if saveErr := SaveBLCache(c.blCache, bl); saveErr != nil {
		c.Logf("BL cache write failed (non-fatal): %v", saveErr)
	}
	return bl, nil
}

// CachedBL returns the persisted BL if it is newer than the currently
// configured one. It never fails and never blocks on the network.
func (c *Client) CachedBL() (string, bool) {
	cached, _, ok := LoadBLCache(c.blCache)
	if !ok {
		return "", false
	}
	current := c.CurrentBL()
	if BLNewer(cached, current) {
		return cached, true
	}
	return "", false
}

// AdoptCachedBL promotes a newer on-disk cached BL into the live client.
// Called at startup so a previously validated label survives restarts even
// when the upstream is blocked. Returns the adopted label, the previous one,
// and whether anything changed.
func (c *Client) AdoptCachedBL() (adopted, previous string, changed bool) {
	cached, ok := c.CachedBL()
	if !ok {
		return "", c.CurrentBL(), false
	}
	previous = c.CurrentBL()
	c.SetBL(cached)
	return cached, previous, true
}

// profile returns the impersonation profile actually backing c.HTTP, or "" when
// the transport is the stdlib fallback. It reads the transport rather than the
// config so a failed TLS-client construction cannot leave the header set
// claiming a browser fingerprint - and browser Accept-Encoding - the transport
// does not have.
func (c *Client) profile() string { return ProfileOf(c.HTTP) }

func (c *Client) buildHeaders() http.Header {
	cookieInfo, _ := c.Cookies.Load()
	authUser := c.Cfg.AuthUser
	if authUser == "" && cookieInfo.AuthUser != "" {
		authUser = cookieInfo.AuthUser
	}

	prefix := AccountPrefix(authUser)

	// Start from the browser header set so UA/Accept/Accept-Language/
	// Accept-Encoding/client hints always agree with the TLS profile in use.
	// c.profile() reads the live transport, so the stdlib fallback is treated as
	// "not impersonating" even when config still names a profile. Accept-Encoding
	// lands on the browser list when impersonating (tls-client decodes those
	// encodings) and on identity for stdlib (its transport has DisableCompression
	// set, so a compressed body would reach the parser undecoded).
	h := BrowserHeaders(c.profile(), false)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Origin", "https://gemini.google.com")
	h.Set("Referer", fmt.Sprintf("https://gemini.google.com%s/app", prefix))
	h.Set("X-Same-Domain", "1")

	if prefix != "" && authUser != "" {
		h.Set("X-Goog-AuthUser", authUser)
	}

	if cookieInfo.Cookie != "" {
		h.Set("Cookie", cookieInfo.Cookie)
	}
	if cookieInfo.SAPISID != "" {
		hash := SAPISIDHash(cookieInfo.SAPISID)
		if hash != "" {
			h.Set("Authorization", hash)
		}
	}

	return h
}

// triageStatus classifies a non-2xx upstream response so the retry loop can
// decide whether another attempt can possibly succeed. Redirects are never
// followed by the transport (CheckRedirect returns ErrUseLastResponse), so a
// 3xx here is the block signal itself, not an unrelated hop.
func (c *Client) triageStatus(resp *http.Response) error {
	loc := resp.Header.Get("Location")

	if isSorryLocation(loc) {
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   KindBlocked,
			Msg: "upstream blocked: IP flagged by Google (redirected to sorry/index). " +
				"Tunggu beberapa saat, ganti proxy, atau pakai --impersonate.",
		}
	}

	if resp.StatusCode == http.StatusMethodNotAllowed {
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   KindBLEpired,
			Msg:    fmt.Sprintf("upstream HTTP 405 for bl=%s (build label rejected)", c.CurrentBL()),
		}
	}

	if isRedirectStatus(resp.StatusCode) {
		// Any other redirect means the session/account path is not serving the
		// app (login wall, account picker, consent). Retrying cannot fix it.
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   KindAuth,
			Msg: fmt.Sprintf("upstream redirected to %s (HTTP %d); cookies/session likely invalid or expired",
				locationOrUnknown(loc), resp.StatusCode),
		}
	}

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   KindRateLimited,
			Msg:    "upstream rate limited (HTTP 429); retry later",
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   KindAuth,
			Msg:    fmt.Sprintf("upstream HTTP %d: authentication rejected (cookies or SAPISID invalid)", resp.StatusCode),
		}
	case http.StatusOK:
		return nil
	}

	return &UpstreamError{
		Status: resp.StatusCode,
		Kind:   KindHTTP,
		Msg:    fmt.Sprintf("upstream HTTP %d", resp.StatusCode),
	}
}

// retryable reports whether an error is worth another attempt on the same IP
// and BL. Blocked/auth/BL-expiry failures are terminal: repeating them only
// deepens an IP block and burns latency.
func retryable(err error) bool {
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		// Parser/dialect errors surface as plain errors; treat as retryable
		// only when they are not a client disconnect (callers decide).
		return true
	}
	switch ue.Kind {
	case KindBlocked, KindAuth, KindBLEpired:
		return false
	}
	return true
}

// maxBLRefreshPerRequest caps how many times one logical request may refresh
// its build label. Two refreshes cover "config BL stale" plus "cache stale"
// without allowing an unbounded loop if Google keeps answering 405.
const maxBLRefreshPerRequest = 2

// retryDelay returns the backoff before attempt+1. Rate limiting gets a
// longer, linear-ish ramp so we stop hammering Google's edge.
func (c *Client) retryDelay(attempt int, err error) time.Duration {
	base := time.Duration(c.Cfg.RetryDelaySec) * time.Second
	if base <= 0 {
		base = time.Second
	}
	var ue *UpstreamError
	if errors.As(err, &ue) && ue.Kind == KindRateLimited {
		return base * time.Duration(attempt+2)
	}
	return base
}

// blRefreshError builds the terminal error returned when a 405 could not be
// resolved by refreshing the build label. It names both the label in use and
// why the refresh failed, so the operator does not have to guess.
func blRefreshError(bl string, refreshErr error) error {
	return &UpstreamError{
		Status: http.StatusMethodNotAllowed,
		Kind:   KindBLEpired,
		Msg: fmt.Sprintf("upstream HTTP 405 for bl=%s and BL refresh failed (%v); "+
			"not retrying with the same stale build label", bl, refreshErr),
	}
}

// refreshBLFor405 refreshes the build label once. It returns the label to use
// and whether a retry makes sense. A successful fetch that yields the same
// label means the config is already current, so retrying is pointless.
func (c *Client) refreshBLFor405(current string) (string, bool, error) {
	newBL, changed, err := c.UpdateBLIfNeeded()
	if err != nil {
		return current, false, err
	}
	if !changed {
		return current, false, fmt.Errorf("upstream still reports the build label %s as invalid", current)
	}
	return newBL, true, nil
}

// isBLError reports whether err is a stale-build-label (HTTP 405) failure.
func isBLError(err error) bool {
	var ue *UpstreamError
	return errors.As(err, &ue) && ue.Kind == KindBLEpired
}

// Generate performs one non-streaming call. ctx bounds the whole request: when
// the caller's client disconnects, the upstream call is cancelled instead of
// running to the timeout and burning quota.
func (c *Client) Generate(ctx context.Context, prompt string, opt ModelOptions, fileRefs []string) (string, error) {
	c.EnsureModelsDiscovered()

	cookieInfo, _ := c.Cookies.Load()
	cfg := c.Cfg
	if cfg.XSRFToken == "" && cookieInfo.XSRFToken != "" {
		cfg.XSRFToken = cookieInfo.XSRFToken
	}
	if cfg.AuthUser == "" && cookieInfo.AuthUser != "" {
		cfg.AuthUser = cookieInfo.AuthUser
	}

	reqUUID := strings.ToUpper(uuidV4())
	extra := make(map[int]any, len(opt.Extra)+1)
	for k, v := range opt.Extra {
		extra[k] = v
	}
	extra[59] = reqUUID

	modelHdr, effectiveMode := c.resolveModelHeader(opt, reqUUID)
	bodyStr := BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
	reqURL := BuildURL(cfg, c.CurrentBL())
	headers := c.buildHeaders()
	headers.Set(HeaderUUIDJspb, fmt.Sprintf(`["%s",1]`, reqUUID))
	if modelHdr != "" {
		headers.Set(ModelHeaderKey, modelHdr)
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	}

	attempts := c.Cfg.RetryAttempts
	if attempts < 1 {
		attempts = 1
	}
	blRefreshes := 0
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", reqURL, strings.NewReader(bodyStr))
		if err != nil {
			return "", err
		}
		req.Header = headers.Clone()

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = &UpstreamError{Kind: KindTransport, Msg: err.Error()}
		} else {
			if terr := c.triageStatus(resp); terr != nil {
				_ = resp.Body.Close()

				// 405 = BL expired. Refresh at most twice; a retry with the
				// same label is guaranteed to fail, so never do it.
				if isBLError(terr) && blRefreshes < maxBLRefreshPerRequest {
					blRefreshes++
					oldBL := c.CurrentBL()
					newBL, ok, refreshErr := c.refreshBLFor405(oldBL)
					if refreshErr != nil || !ok {
						return "", blRefreshError(oldBL, refreshErr)
					}
					c.Logf("BL auto-updated (%s -> %s), retrying request", oldBL, newBL)
					bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
					reqURL = BuildURL(cfg, newBL)
					attempt-- // a BL refresh does not consume a retry
					continue
				}
				if isBLError(terr) {
					return "", blRefreshError(c.CurrentBL(),
						fmt.Errorf("build label refreshed %d times and upstream still rejects it", blRefreshes))
				}
				// Blocked IP / auth failures are terminal: retrying from the
				// same IP cannot change the outcome and prolongs the block.
				if !retryable(terr) {
					return "", terr
				}
				lastErr = terr
			} else {
				rawBytes, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					lastErr = &UpstreamError{Kind: KindTransport, Msg: err.Error()}
				} else {
					text, err := ExtractResponseText(string(rawBytes))
					if err != nil {
						lastErr = &UpstreamError{Kind: KindBard, Msg: err.Error()}
					} else {
						return text, nil
					}
				}
			}
		}

		if attempt < attempts-1 {
			c.Logf("Retry %d/%d: %v", attempt+1, attempts, lastErr)
			time.Sleep(c.retryDelay(attempt, lastErr))
		}
	}

	return "", lastErr
}

// GenerateStream performs one streaming call. ctx cancels the upstream request
// when the caller stops reading, so a disconnected client does not hold the
// connection open until the timeout.
func (c *Client) GenerateStream(ctx context.Context, prompt string, opt ModelOptions, fileRefs []string, emit func(string) error) error {
	c.EnsureModelsDiscovered()

	cookieInfo, _ := c.Cookies.Load()
	cfg := c.Cfg
	if cfg.XSRFToken == "" && cookieInfo.XSRFToken != "" {
		cfg.XSRFToken = cookieInfo.XSRFToken
	}
	if cfg.AuthUser == "" && cookieInfo.AuthUser != "" {
		cfg.AuthUser = cookieInfo.AuthUser
	}

	reqUUID := strings.ToUpper(uuidV4())
	extra := make(map[int]any, len(opt.Extra)+1)
	for k, v := range opt.Extra {
		extra[k] = v
	}
	extra[59] = reqUUID

	modelHdr, effectiveMode := c.resolveModelHeader(opt, reqUUID)
	bodyStr := BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
	reqURL := BuildURL(cfg, c.CurrentBL())
	headers := c.buildHeaders()
	headers.Set(HeaderUUIDJspb, fmt.Sprintf(`["%s",1]`, reqUUID))
	if modelHdr != "" {
		headers.Set(ModelHeaderKey, modelHdr)
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	}

	attempts := c.Cfg.RetryAttempts
	if attempts < 1 {
		attempts = 1
	}
	blRefreshes := 0
	parser := NewStreamParser()
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		parser.ResetBuffer()

		req, err := http.NewRequestWithContext(ctx, "POST", reqURL, strings.NewReader(bodyStr))
		if err != nil {
			return err
		}
		req.Header = headers.Clone()

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = &UpstreamError{Kind: KindTransport, Msg: err.Error()}
		} else {
			if terr := c.triageStatus(resp); terr != nil {
				_ = resp.Body.Close()

				// 405 = BL expired. Refresh at most twice; never retry with a
				// build label Google has already rejected.
				if isBLError(terr) && blRefreshes < maxBLRefreshPerRequest {
					blRefreshes++
					oldBL := c.CurrentBL()
					newBL, ok, refreshErr := c.refreshBLFor405(oldBL)
					if refreshErr != nil || !ok {
						return blRefreshError(oldBL, refreshErr)
					}
					c.Logf("BL auto-updated (%s -> %s), retrying stream request", oldBL, newBL)
					bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
					reqURL = BuildURL(cfg, newBL)
					attempt-- // a BL refresh does not consume a retry
					continue
				}
				if isBLError(terr) {
					return blRefreshError(c.CurrentBL(),
						fmt.Errorf("build label refreshed %d times and upstream still rejects it", blRefreshes))
				}
				if !retryable(terr) {
					return terr
				}
				lastErr = terr
			} else {
				err = c.streamAttempt(resp.Body, parser, emit)
				_ = resp.Body.Close()
				if err == nil {
					return nil
				}
				if isClientDisconnect(err) {
					return err
				}
				lastErr = err
			}
		}

		if attempt < attempts-1 {
			c.Logf("Stream retry %d/%d: %v", attempt+1, attempts, lastErr)
			time.Sleep(c.retryDelay(attempt, lastErr))
		}
	}

	return lastErr
}

func (c *Client) streamAttempt(body io.Reader, parser *StreamParser, emit func(string) error) error {
	reader := bufio.NewReader(body)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if len(lineBytes) > 0 {
			deltas, parseErr := parser.Feed(string(lineBytes))
			if parseErr != nil {
				return &UpstreamError{Kind: KindBard, Msg: parseErr.Error()}
			}
			for _, delta := range deltas {
				if err := emit(delta); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return &UpstreamError{Kind: KindTransport, Msg: err.Error()}
		}
	}
}

// isClientDisconnect helps distinguish between upstream server errors (which we might want to retry)
// and actual client-side disconnections or non-retryable transport errors.
// If the client disconnected, we should abort the retry loop immediately to save resources.
func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*UpstreamError); ok {
		return false
	}
	return true
}
