package gemini

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	fhttp "github.com/bogdanfinn/fhttp"
	http_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// browserFamily groups impersonation profiles by the header dialect they must
// speak. A Chrome TLS fingerprint paired with a Firefox User-Agent is just as
// inconsistent as a truncated UA, so the UA, Accept*, client hints and header
// order are all derived from the family instead of being written by hand at
// each call site.
type browserFamily int

const (
	familyChrome browserFamily = iota
	familyFirefox
	familySafari
)

// DefaultProfileName is the profile used for unknown names and for the stdlib
// path (no impersonation). It is the newest profile in the registry, so its UA
// also matches the default TLS fingerprint.
const DefaultProfileName = "chrome_146"

// profileInfo is one row of the impersonation registry: the TLS fingerprint plus
// the header metadata that has to agree with it.
type profileInfo struct {
	Family         browserFamily
	TLS            profiles.ClientProfile
	Version        string // browser major version, e.g. "146"
	UserAgent      string
	AcceptEncoding string
}

// profileRegistry is the single source of truth for supported impersonation
// profiles. Adding a profile means adding one row here (plus test coverage),
// not editing every call site that needs a User-Agent.
var profileRegistry = map[string]profileInfo{
	"chrome_120": {
		Family: familyChrome, TLS: profiles.Chrome_120, Version: "120",
		UserAgent: chromeUserAgent("120"), AcceptEncoding: chromeAcceptEncoding("120"),
	},
	"chrome_124": {
		Family: familyChrome, TLS: profiles.Chrome_124, Version: "124",
		UserAgent: chromeUserAgent("124"), AcceptEncoding: chromeAcceptEncoding("124"),
	},
	"chrome_131": {
		Family: familyChrome, TLS: profiles.Chrome_131, Version: "131",
		UserAgent: chromeUserAgent("131"), AcceptEncoding: chromeAcceptEncoding("131"),
	},
	"chrome_133": {
		Family: familyChrome, TLS: profiles.Chrome_133, Version: "133",
		UserAgent: chromeUserAgent("133"), AcceptEncoding: chromeAcceptEncoding("133"),
	},
	"chrome_144": {
		Family: familyChrome, TLS: profiles.Chrome_144, Version: "144",
		UserAgent: chromeUserAgent("144"), AcceptEncoding: chromeAcceptEncoding("144"),
	},
	"chrome_146": {
		Family: familyChrome, TLS: profiles.Chrome_146, Version: "146",
		UserAgent: chromeUserAgent("146"), AcceptEncoding: chromeAcceptEncoding("146"),
	},
	"firefox_120": {
		Family: familyFirefox, TLS: profiles.Firefox_120, Version: "120",
		UserAgent: firefoxUserAgent("120"), AcceptEncoding: "gzip, deflate, br",
	},
	"firefox_123": {
		Family: familyFirefox, TLS: profiles.Firefox_123, Version: "123",
		UserAgent: firefoxUserAgent("123"), AcceptEncoding: "gzip, deflate, br",
	},
	"firefox_147": {
		Family: familyFirefox, TLS: profiles.Firefox_147, Version: "147",
		UserAgent: firefoxUserAgent("147"), AcceptEncoding: "gzip, deflate, br, zstd",
	},
	"safari_16_0": {
		Family: familySafari, TLS: profiles.Safari_16_0, Version: "16.0",
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Safari/605.1.15",
		AcceptEncoding: "gzip, deflate, br",
	},
	"safari_ios_17_0": {
		Family: familySafari, TLS: profiles.Safari_IOS_17_0, Version: "17.0",
		UserAgent:      "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
		AcceptEncoding: "gzip, deflate, br",
	},
}

// profileNames is the sorted, stable list of valid profile names. It backs both
// diagnostics and the unknown-name warning, so it is built once from the
// registry instead of being maintained by hand.
var profileNames = func() []string {
	names := make([]string, 0, len(profileRegistry))
	for name := range profileRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}()

// chromeUserAgent builds a complete Chrome UA. The trailing
// "Chrome/<version>.0.0.0 Safari/537.36" segment is mandatory: a string that
// stops at "AppleWebKit/537.36" is not something any browser ever sends.
func chromeUserAgent(version string) string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" +
		version + ".0.0.0 Safari/537.36"
}

func firefoxUserAgent(version string) string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:" + version + ".0) Gecko/20100101 Firefox/" +
		version + ".0"
}

// chromeAcceptEncoding mirrors the list Chrome advertises. zstd joined the list
// in Chrome 123, so older profiles must not claim it.
func chromeAcceptEncoding(version string) string {
	if n, err := strconv.Atoi(version); err == nil && n >= 123 {
		return "gzip, deflate, br, zstd"
	}
	return "gzip, deflate, br"
}

func (p profileInfo) acceptLanguage() string {
	if p.Family == familyFirefox {
		return "en-US,en;q=0.5"
	}
	return "en-US,en;q=0.9"
}

// accept returns the Accept header for a top-level document navigation (BL
// scrape) or for an XHR/fetch call (batchexecute). Browsers send a media-rich
// list for documents and a bare "*/*" for scripted requests.
func (p profileInfo) accept(document bool) string {
	if !document {
		return "*/*"
	}
	switch p.Family {
	case familyChrome:
		return "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"
	default:
		return "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	}
}

// clientHints returns the Sec-CH-UA* headers Chromium sends. Firefox and Safari
// do not send them, so returning nil there avoids advertising a Chromium-only
// header from a non-Chromium profile.
func (p profileInfo) clientHints() map[string]string {
	if p.Family != familyChrome {
		return nil
	}
	v := p.Version
	return map[string]string{
		"Sec-CH-UA":          fmt.Sprintf(`"Chromium";v="%s", "Not-A.Brand";v="24", "Google Chrome";v="%s"`, v, v),
		"Sec-CH-UA-Mobile":   "?0",
		"Sec-CH-UA-Platform": `"Windows"`,
	}
}

// Header orders below are per family. Exact ordering is not security-relevant,
// but a stable, plausible order keeps the request shape consistent with the TLS
// fingerprint instead of leaking Go's lexicographic map ordering.
var (
	chromeHeaderOrder = []string{
		"host", "content-length", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
		"upgrade-insecure-requests", "user-agent", "accept", "origin", "sec-fetch-site",
		"sec-fetch-mode", "sec-fetch-dest", "referer", "x-goog-authuser", "x-same-domain",
		"authorization", "content-type", "accept-encoding", "accept-language", "cookie",
	}
	firefoxHeaderOrder = []string{
		"host", "content-length", "user-agent", "accept", "accept-language", "accept-encoding",
		"content-type", "origin", "referer", "x-goog-authuser", "x-same-domain",
		"authorization", "cookie",
	}
	safariHeaderOrder = []string{
		"host", "content-length", "accept", "user-agent", "content-type", "origin", "referer",
		"accept-language", "accept-encoding", "x-goog-authuser", "x-same-domain",
		"authorization", "cookie",
	}
)

// headerOrder returns the wire header order for the family. The slice is shared
// and must be treated as read-only.
func (p profileInfo) headerOrder() []string {
	switch p.Family {
	case familyFirefox:
		return firefoxHeaderOrder
	case familySafari:
		return safariHeaderOrder
	default:
		return chromeHeaderOrder
	}
}

// BrowserHeaders returns the browser header set for profile. An empty profile
// means "no impersonation": the stdlib transport is in play, its DisableCompression
// is on, and Accept-Encoding must stay identity because nothing would decode a
// compressed body. When a profile is given, Accept-Encoding mirrors what that
// browser sends (the tls-client transport decompresses gzip/deflate/br/zstd), so
// the header set never contradicts the TLS fingerprint.
//
// document selects a top-level navigation (Accept for HTML) versus an XHR
// (Accept "*/*", Sec-Fetch-Mode cors). Callers add request-specific headers on
// top of the result.
func BrowserHeaders(profile string, document bool) http.Header {
	h := make(http.Header)
	info, impersonating := profileInfoFor(profile)

	h.Set("User-Agent", info.UserAgent)
	h.Set("Accept", info.accept(document))
	h.Set("Accept-Language", info.acceptLanguage())
	if impersonating {
		h.Set("Accept-Encoding", info.AcceptEncoding)
	} else {
		h.Set("Accept-Encoding", "identity")
	}
	if document {
		h.Set("Upgrade-Insecure-Requests", "1")
	}

	for k, v := range info.clientHints() {
		h.Set(k, v)
	}

	// Sec-Fetch-* is Chromium-only; sending it from a Firefox/Safari profile
	// would be a fresh mismatch.
	if info.Family == familyChrome {
		if document {
			h.Set("Sec-Fetch-Site", "none")
			h.Set("Sec-Fetch-Mode", "navigate")
			h.Set("Sec-Fetch-Dest", "document")
		} else {
			h.Set("Sec-Fetch-Site", "same-origin")
			h.Set("Sec-Fetch-Mode", "cors")
			h.Set("Sec-Fetch-Dest", "empty")
		}
	}
	return h
}

// profileInfoFor resolves a profile name and reports whether impersonation is
// actually in effect.
func profileInfoFor(profile string) (profileInfo, bool) {
	key := strings.ToLower(strings.TrimSpace(profile))
	if key == "" {
		return profileRegistry[DefaultProfileName], false
	}
	return lookupProfile(key), true
}

// warnedUnknownProfiles keeps the unknown-profile warning to one line per name
// so a misconfigured name does not flood the log on every request.
var warnedUnknownProfiles sync.Map

// lookupProfile resolves a profile name to its registry row. Unknown names fall
// back to the default profile and warn once: silently substituting a different
// fingerprint would hide a typo in config.json or on the command line.
func lookupProfile(name string) profileInfo {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return profileRegistry[DefaultProfileName]
	}
	if info, ok := profileRegistry[key]; ok {
		return info
	}
	if _, alreadyWarned := warnedUnknownProfiles.LoadOrStore(key, struct{}{}); !alreadyWarned {
		log.Printf("WARNING: unknown impersonate profile %q; falling back to %s. Valid profiles: %s",
			name, DefaultProfileName, strings.Join(KnownProfiles(), ", "))
	}
	return profileRegistry[DefaultProfileName]
}

// KnownProfiles lists every supported impersonation profile name, sorted.
func KnownProfiles() []string {
	out := make([]string, len(profileNames))
	copy(out, profileNames)
	return out
}

// IsKnownProfile reports whether name is a supported impersonation profile.
func IsKnownProfile(name string) bool {
	_, ok := profileRegistry[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// UserAgent returns the browser User-Agent that matches profile. An empty or
// unknown profile yields the default Chrome UA, matching the default TLS
// fingerprint; a UA never stops at "AppleWebKit/537.36".
func UserAgent(profile string) string { return lookupProfile(profile).UserAgent }

// ProfileOf reports the impersonation profile behind a Requester, or "" when the
// requester is not an impersonating client (stdlib path). Callers that build
// their own headers use it to keep the User-Agent consistent with the transport
// instead of hardcoding a string.
func ProfileOf(req Requester) string {
	if adapter, ok := req.(*tlsClientAdapter); ok {
		return adapter.profileName
	}
	return ""
}

// resolveProfile returns the TLS fingerprint for a profile name. Unknown names
// warn and fall back to the default profile (see lookupProfile), so a typo is
// visible in the log instead of being silently replaced by a different
// fingerprint.
func resolveProfile(name string) profiles.ClientProfile {
	return lookupProfile(name).TLS
}

type tlsClientAdapter struct {
	client      http_client.HttpClient
	profileName string
	headerOrder []string
}

var (
	clientMap   = make(map[string]*tlsClientAdapter)
	clientMapMu sync.Mutex
)

// canonicalProfileName normalizes a profile name to its registry key. Unknown
// names collapse to the default so they share one cached client instead of
// creating a new one per typo.
func canonicalProfileName(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return DefaultProfileName
	}
	if _, ok := profileRegistry[key]; !ok {
		return DefaultProfileName
	}
	return key
}

// tlsClientCacheKey builds the identity of a cached impersonating client. The
// proxy belongs in the key: each tls-client owns its dialer, so reusing one for
// a second proxy would silently route traffic through the first proxy.
func tlsClientCacheKey(profileName string, timeoutSec int, proxy string) string {
	return fmt.Sprintf("%s:%d:%s", canonicalProfileName(profileName), timeoutSec, proxy)
}

// getTLSClient returns a cached impersonating client for (profile, timeout,
// proxy). The proxy is part of the cache key because every client owns its own
// dialer and connection pool: two clients built for different proxies are not
// interchangeable.
func getTLSClient(profileName string, timeoutSec int, proxy string) (*tlsClientAdapter, error) {
	clientMapMu.Lock()
	defer clientMapMu.Unlock()

	cacheKey := tlsClientCacheKey(profileName, timeoutSec, proxy)
	if adapter, ok := clientMap[cacheKey]; ok {
		return adapter, nil
	}

	info := lookupProfile(profileName)
	options := []http_client.HttpClientOption{
		http_client.WithClientProfile(resolveProfile(profileName)),
		http_client.WithTimeoutSeconds(timeoutSec),
		// Block detection relies on seeing the 302 to sorry/index itself, so the
		// transport must never follow redirects.
		http_client.WithNotFollowRedirects(),
	}
	if proxy != "" {
		options = append(options, http_client.WithProxyUrl(proxy))
	}

	client, err := http_client.NewHttpClient(http_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}

	adapter := &tlsClientAdapter{
		client:      client,
		profileName: canonicalProfileName(profileName),
		headerOrder: info.headerOrder(),
	}
	clientMap[cacheKey] = adapter
	return adapter, nil
}

func (a *tlsClientAdapter) Do(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	freq, err := fhttp.NewRequest(req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	for k, vv := range req.Header {
		for _, v := range vv {
			freq.Header.Add(k, v)
		}
	}

	// fhttp reads the order from a magic header key and strips it before writing,
	// so the order stays a transport detail instead of a wire header.
	if len(a.headerOrder) > 0 {
		freq.Header[fhttp.HeaderOrderKey] = a.headerOrder
	}

	fresp, err := a.client.Do(freq)
	if err != nil {
		return nil, err
	}

	resp := &http.Response{
		Status:        fresp.Status,
		StatusCode:    fresp.StatusCode,
		Proto:         fresp.Proto,
		ProtoMajor:    fresp.ProtoMajor,
		ProtoMinor:    fresp.ProtoMinor,
		Header:        make(http.Header),
		Body:          fresp.Body,
		ContentLength: fresp.ContentLength,
		Request:       req,
	}

	for k, vv := range fresp.Header {
		for _, v := range vv {
			resp.Header.Add(k, v)
		}
	}

	return resp, nil
}
