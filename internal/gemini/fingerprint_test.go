package gemini

import (
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

// TestNewClientImpersonateWithProxy pins the bug fix: a configured proxy must
// not disable TLS impersonation. The Requester type is the observable signal -
// the impersonating path returns *tlsClientAdapter, the fallback returns
// *http.Client.
func TestNewClientImpersonateWithProxy(t *testing.T) {
	t.Setenv(BLCachePathEnv, filepath.Join(t.TempDir(), "bl.json"))

	cases := []struct {
		name        string
		impersonate string
		proxy       string
		wantTLS     bool
	}{
		{"impersonate only", "chrome_146", "", true},
		{"impersonate plus proxy", "chrome_146", "http://127.0.0.1:60000", true},
		{"proxy only falls back to stdlib", "", "http://127.0.0.1:60000", false},
		{"neither falls back to stdlib", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Impersonate = tc.impersonate
			cfg.Proxy = tc.proxy
			c := NewClient(cfg)

			_, isTLS := c.HTTP.(*tlsClientAdapter)
			if isTLS != tc.wantTLS {
				t.Fatalf("requester type = %T, want impersonating=%v", c.HTTP, tc.wantTLS)
			}
		})
	}
}

// TestTLSClientProxyPassedThrough checks the proxy actually reaches the cached
// client, i.e. two proxies do not share one client.
func TestTLSClientProxyPassedThrough(t *testing.T) {
	clientMapMu.Lock()
	clientMap = make(map[string]*tlsClientAdapter)
	clientMapMu.Unlock()

	a, err := getTLSClient("chrome_146", 30, "http://127.0.0.1:60000")
	if err != nil {
		t.Fatalf("getTLSClient without proxy error: %v", err)
	}
	b, err := getTLSClient("chrome_146", 30, "http://127.0.0.1:60001")
	if err != nil {
		t.Fatalf("getTLSClient with proxy error: %v", err)
	}
	if a == b {
		t.Fatal("clients for different proxies must not be the same instance")
	}

	again, err := getTLSClient("chrome_146", 30, "http://127.0.0.1:60001")
	if err != nil {
		t.Fatalf("getTLSClient repeat error: %v", err)
	}
	if again != b {
		t.Fatal("same (profile, timeout, proxy) must hit the cache")
	}
}

// TestTLSCacheKeyIncludesProxy is the regression guard for the latent cache
// collision: changing only the proxy must change the key.
func TestTLSCacheKeyIncludesProxy(t *testing.T) {
	base := tlsClientCacheKey("chrome_146", 30, "")
	withProxy := tlsClientCacheKey("chrome_146", 30, "http://127.0.0.1:60000")
	otherProxy := tlsClientCacheKey("chrome_146", 30, "http://127.0.0.1:60001")

	if base == withProxy || withProxy == otherProxy {
		t.Fatalf("cache keys must differ per proxy: %q %q %q", base, withProxy, otherProxy)
	}
	if base != tlsClientCacheKey("chrome_146", 30, "") {
		t.Fatal("cache key must be stable for identical inputs")
	}
}

// TestUserAgentMatchesProfile asserts every registered profile ships a complete,
// version-consistent UA. A UA ending at "AppleWebKit/537.36" is the bug being
// fixed, so it is checked explicitly.
func TestUserAgentMatchesProfile(t *testing.T) {
	for name := range profileRegistry {
		ua := UserAgent(name)
		if ua == "" {
			t.Errorf("%s: empty User-Agent", name)
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(ua), "AppleWebKit/537.36") {
			t.Errorf("%s: truncated User-Agent %q", name, ua)
		}
		if !strings.Contains(ua, "Mozilla/5.0") {
			t.Errorf("%s: UA is not a browser UA: %q", name, ua)
		}

		info := profileRegistry[name]
		switch info.Family {
		case familyChrome:
			if !strings.Contains(ua, "Chrome/"+info.Version+".0.0.0") {
				t.Errorf("%s: UA %q does not carry Chrome/%s.0.0.0", name, ua, info.Version)
			}
			if !strings.Contains(ua, "Safari/537.36") {
				t.Errorf("%s: Chrome UA %q is missing the Safari/537.36 tail", name, ua)
			}
		case familyFirefox:
			if !strings.Contains(ua, "Firefox/"+info.Version+".0") {
				t.Errorf("%s: UA %q does not carry Firefox/%s.0", name, ua, info.Version)
			}
		case familySafari:
			if !strings.Contains(ua, "Safari/") || !strings.Contains(ua, "Version/"+info.Version) {
				t.Errorf("%s: UA %q does not look like Safari %s", name, ua, info.Version)
			}
		}
	}
}

// TestUserAgentVersionMapping guards the concrete examples from the report.
func TestUserAgentVersionMapping(t *testing.T) {
	want := map[string]string{
		"chrome_120":  "Chrome/120.0.0.0",
		"chrome_131":  "Chrome/131.0.0.0",
		"chrome_146":  "Chrome/146.0.0.0",
		"firefox_120": "Firefox/120.0",
		"firefox_147": "Firefox/147.0",
		"safari_16_0": "Version/16.0",
	}
	for profile, fragment := range want {
		if ua := UserAgent(profile); !strings.Contains(ua, fragment) {
			t.Errorf("UserAgent(%q) = %q, want it to contain %q", profile, ua, fragment)
		}
	}
}

// TestUserAgentDefaultIsComplete covers the stdlib path: no profile must still
// produce a full Chrome UA, never the truncated string.
func TestUserAgentDefaultIsComplete(t *testing.T) {
	ua := UserAgent("")
	if !strings.Contains(ua, "Chrome/") || !strings.HasSuffix(ua, "Safari/537.36") {
		t.Fatalf("default UA is not a complete Chrome UA: %q", ua)
	}
	if strings.HasSuffix(ua, "AppleWebKit/537.36") {
		t.Fatalf("default UA is truncated: %q", ua)
	}
}

// TestUnknownProfileWarnsAndFallsBack verifies a typo is loud but harmless: the
// default profile is used and the warning names both the bad name and the list
// of valid ones. It must not panic.
func TestUnknownProfileWarnsAndFallsBack(t *testing.T) {
	// The "warn once" cache is process-wide, so clear it to keep this test
	// independent of test ordering.
	warnedUnknownProfiles = sync.Map{}

	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	got := lookupProfile("chrome_999_typo")
	if want := profileRegistry[DefaultProfileName]; got.UserAgent != want.UserAgent || got.Version != want.Version {
		t.Fatalf("unknown profile must fall back to the default profile, got %q", got.UserAgent)
	}

	out := buf.String()
	if !strings.Contains(out, "chrome_999_typo") {
		t.Errorf("warning should name the unknown profile, got: %q", out)
	}
	if !strings.Contains(out, "chrome_146") {
		t.Errorf("warning should list valid profiles, got: %q", out)
	}
	if !strings.Contains(out, "Valid profiles:") {
		t.Errorf("warning should point at the valid profile set, got: %q", out)
	}
}

// TestKnownProfilesAreStable guards the introspection helpers used by main.go.
func TestKnownProfilesAreStable(t *testing.T) {
	names := KnownProfiles()
	if len(names) != len(profileRegistry) {
		t.Fatalf("KnownProfiles() returned %d names, registry has %d", len(names), len(profileRegistry))
	}
	for _, n := range names {
		if !IsKnownProfile(n) {
			t.Errorf("KnownProfiles() lists %q but IsKnownProfile says otherwise", n)
		}
	}
	if IsKnownProfile("not_a_profile") {
		t.Error("IsKnownProfile must reject unknown names")
	}
	// The default must itself be valid, since it is the fallback target.
	if !IsKnownProfile(DefaultProfileName) {
		t.Errorf("DefaultProfileName %q is not in the registry", DefaultProfileName)
	}
}

// TestBuildHeadersUsesProfile checks the chat header set follows the transport:
// a browser Accept-Encoding when impersonating, identity on the stdlib path, and
// a UA with a real Chrome version.
func TestBuildHeadersUsesProfile(t *testing.T) {
	t.Setenv(BLCachePathEnv, filepath.Join(t.TempDir(), "bl.json"))

	t.Run("impersonating", func(t *testing.T) {
		cfg := config.Default()
		cfg.Impersonate = "chrome_146"
		c := NewClient(cfg)
		h := c.buildHeaders()

		if got := h.Get("User-Agent"); !strings.Contains(got, "Chrome/146.0.0.0") {
			t.Errorf("User-Agent = %q, want a complete Chrome/146 UA", got)
		}
		ae := h.Get("Accept-Encoding")
		if ae == "identity" || ae == "" {
			t.Errorf("Accept-Encoding = %q, want the browser encoding list when impersonating", ae)
		}
		if !strings.Contains(ae, "gzip") {
			t.Errorf("Accept-Encoding = %q, want gzip in the list", ae)
		}
		if got := h.Get("Accept"); got != "*/*" {
			t.Errorf("Accept = %q, want */* for an XHR", got)
		}
	})

	t.Run("stdlib", func(t *testing.T) {
		c := NewClient(config.Default())
		h := c.buildHeaders()

		if got := h.Get("User-Agent"); !strings.Contains(got, "Chrome/") {
			t.Errorf("User-Agent = %q, want a complete Chrome UA on the stdlib path too", got)
		}
		if got := h.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity on the stdlib path", got)
		}
	})
}

// TestBLHeadersMatchChatHeaders is the "bl.go must use the same UA" requirement.
func TestBLHeadersMatchChatHeaders(t *testing.T) {
	t.Setenv(BLCachePathEnv, filepath.Join(t.TempDir(), "bl.json"))

	cfg := config.Default()
	cfg.Impersonate = "chrome_131"
	c := NewClient(cfg)
	chatUA := c.buildHeaders().Get("User-Agent")

	for _, withAuth := range []bool{false, true} {
		blUA := BLFetchOptions{Profile: cfg.Impersonate}.headers(withAuth).Get("User-Agent")
		if blUA != chatUA {
			t.Errorf("BL UA (withAuth=%v) = %q, want the chat UA %q", withAuth, blUA, chatUA)
		}
		if !strings.Contains(blUA, "Chrome/131.0.0.0") {
			t.Errorf("BL UA = %q, want Chrome/131.0.0.0", blUA)
		}
	}

	// The BL document request keeps an HTML Accept, not */*.
	if got := (BLFetchOptions{Profile: cfg.Impersonate}).headers(false).Get("Accept"); !strings.Contains(got, "text/html") {
		t.Errorf("BL Accept = %q, want an HTML document accept", got)
	}
}

// TestBrowserHeadersNoChromiumHintsOnFirefox avoids a new UA/header mismatch:
// Firefox and Safari must not advertise Sec-CH-UA*.
func TestBrowserHeadersNoChromiumHintsOnFirefox(t *testing.T) {
	for _, profile := range []string{"firefox_147", "safari_16_0", "safari_ios_17_0"} {
		h := BrowserHeaders(profile, true)
		if v := h.Get("Sec-CH-UA"); v != "" {
			t.Errorf("%s: Sec-CH-UA = %q, want empty for a non-Chromium profile", profile, v)
		}
		if v := h.Get("Sec-Fetch-Mode"); v != "" {
			t.Errorf("%s: Sec-Fetch-Mode = %q, want empty for a non-Chromium profile", profile, v)
		}
	}

	h := BrowserHeaders("chrome_146", true)
	if h.Get("Sec-CH-UA") == "" || h.Get("Sec-Fetch-Mode") != "navigate" {
		t.Error("Chrome profile must send client hints and a navigate Sec-Fetch-Mode")
	}
	if !strings.Contains(h.Get("Sec-CH-UA"), "146") {
		t.Errorf("Sec-CH-UA = %q, want the profile version", h.Get("Sec-CH-UA"))
	}
}

// TestProxyOfRequester covers the helper multimodal uses to keep its UA in sync.
func TestProxyOfRequester(t *testing.T) {
	t.Setenv(BLCachePathEnv, filepath.Join(t.TempDir(), "bl.json"))

	cfg := config.Default()
	cfg.Impersonate = "chrome_124"
	c := NewClient(cfg)
	if got := ProfileOf(c.HTTP); got != "chrome_124" {
		t.Errorf("ProfileOf(impersonating) = %q, want chrome_124", got)
	}

	plain := NewClient(config.Default())
	if got := ProfileOf(plain.HTTP); got != "" {
		t.Errorf("ProfileOf(stdlib) = %q, want empty", got)
	}
	if got := ProfileOf(nil); got != "" {
		t.Errorf("ProfileOf(nil) = %q, want empty", got)
	}
}

// TestNewClientAllProfilesWithProxy is a sanity check that constructing a client
// for every registry profile (each with a proxy) never fails or panics.
func TestNewClientAllProfilesWithProxy(t *testing.T) {
	t.Setenv(BLCachePathEnv, filepath.Join(t.TempDir(), "bl.json"))

	for _, name := range KnownProfiles() {
		cfg := config.Default()
		cfg.Impersonate = name
		cfg.Proxy = "http://127.0.0.1:60000"
		c := NewClient(cfg)
		if _, ok := c.HTTP.(*tlsClientAdapter); !ok {
			t.Errorf("%s: expected a TLS client even with a proxy, got %T", name, c.HTTP)
		}
	}
}
