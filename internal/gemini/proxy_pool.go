package gemini

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	proxypool "github.com/ikhsan3adi/gemini-web2api/internal/proxy"
)

// Cooldowns keep a failing proxy out of rotation long enough for the operator
// (or the proxy provider) to recover, without ever removing it permanently.
// When every proxy is cooling, the pool either uses a cooling proxy (fallback
// disabled) or the direct fallback (enabled), so a bad health signal can delay
// traffic but cannot take the service down.
const (
	proxyTransportCooldown = 30 * time.Second
	proxyRateLimitCooldown = 2 * time.Minute
	proxyBlockCooldown     = 5 * time.Minute
)

type proxyRequesterEntry struct {
	proxyURL     string
	requester    Requester
	cooldownTill atomic.Int64 // Unix nanoseconds; 0 means healthy
}

// proxyRequesterPool rotates independent HTTP requesters, one per proxy.
//
// Each requester owns its own transport, so a tls-client built for proxy A is
// never reused for proxy B. That keeps per-proxy connection pools and TLS
// sessions reusable while still spreading requests across proxies.
type proxyRequesterPool struct {
	entries []*proxyRequesterEntry
	next    atomic.Uint64
	profile string
	now     func() time.Time

	// fallback is the optional direct requester. It is never part of normal
	// round-robin; it is used only while every proxy is cooling down.
	fallback       Requester
	fallbackActive atomic.Bool
}

func newProxyRequesterPool(entries []*proxyRequesterEntry, profile string, fallback Requester) *proxyRequesterPool {
	cp := make([]*proxyRequesterEntry, len(entries))
	copy(cp, entries)
	return &proxyRequesterPool{
		entries:  cp,
		profile:  profile,
		now:      time.Now,
		fallback: fallback,
	}
}

// Do sends the request through the next healthy proxy in round-robin order.
// While every proxy is cooling, it uses the direct fallback when configured.
func (p *proxyRequesterPool) Do(req *http.Request) (*http.Response, error) {
	entry, useFallback := p.pick()
	if useFallback {
		return p.fallback.Do(req)
	}
	if entry == nil {
		return nil, fmt.Errorf("no proxy requesters configured")
	}
	resp, err := entry.requester.Do(req)
	p.record(entry, resp, err)
	return resp, err
}

// Profile reports the TLS impersonation profile shared by every requester in
// the pool. It is empty when the pool uses the stdlib transport fallback.
func (p *proxyRequesterPool) Profile() string { return p.profile }

// Len reports the number of active proxies in the pool. The direct fallback is
// deliberately excluded: it must never count as a normal rotation slot.
func (p *proxyRequesterPool) Len() int { return len(p.entries) }

// HasFallback reports whether direct traffic is allowed when every proxy is
// cooling.
func (p *proxyRequesterPool) HasFallback() bool { return p.fallback != nil }

// pick applies round-robin while skipping proxies in cooldown. When every proxy
// is cooling it returns the direct fallback if configured; otherwise it keeps
// using a cooling proxy so the pool fails open rather than stopping traffic.
func (p *proxyRequesterPool) pick() (*proxyRequesterEntry, bool) {
	n := len(p.entries)
	if n == 0 {
		return nil, p.fallback != nil
	}

	start := int((p.next.Add(1) - 1) % uint64(n))
	now := p.now().UnixNano()
	for i := 0; i < n; i++ {
		entry := p.entries[(start+i)%n]
		if entry.cooldownTill.Load() <= now {
			p.markProxyActive()
			return entry, false
		}
	}

	if p.fallback != nil {
		if p.fallbackActive.CompareAndSwap(false, true) {
			log.Printf("All %d proxies are cooling down; using direct fallback", n)
		}
		return nil, true
	}
	return p.entries[start], false
}

func (p *proxyRequesterPool) markProxyActive() {
	if p.fallbackActive.CompareAndSwap(true, false) {
		log.Printf("A proxy is available again; resuming proxy rotation")
	}
}

func (p *proxyRequesterPool) record(entry *proxyRequesterEntry, resp *http.Response, err error) {
	now := p.now()
	if err != nil {
		// A client disconnect says nothing about the proxy's health.
		if errors.Is(err, context.Canceled) {
			return
		}
		p.cool(entry, now, proxyTransportCooldown, "transport error")
		return
	}
	if resp == nil {
		return
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		p.cool(entry, now, proxyRateLimitCooldown, "HTTP 429")
	case resp.StatusCode >= 500:
		p.cool(entry, now, proxyTransportCooldown, fmt.Sprintf("HTTP %d", resp.StatusCode))
	case resp.StatusCode >= 300 && resp.StatusCode < 400 &&
		isSorryLocation(resp.Header.Get("Location")):
		p.cool(entry, now, proxyBlockCooldown, "IP block")
	default:
		// Any other response proves the proxy reached the upstream, so clear a
		// previous cooldown. Account-wide errors (401/403/405) are not the
		// proxy's fault.
		entry.cooldownTill.Store(0)
	}
}

func (p *proxyRequesterPool) cool(entry *proxyRequesterEntry, now time.Time, d time.Duration, reason string) {
	till := now.Add(d).UnixNano()
	prev := entry.cooldownTill.Load()
	if prev >= till {
		return
	}
	if !entry.cooldownTill.CompareAndSwap(prev, till) {
		return
	}
	log.Printf("Proxy %s cooling down for %s after %s", proxypool.Redact(entry.proxyURL), d, reason)
}

// newRequester builds the outbound transport for cfg. A single proxy keeps the
// direct requester path; multiple proxies share one round-robin pool.
//
// TLS-client construction failures are handled per proxy: the failed proxy is
// skipped so the pool never mixes browser-impersonating and stdlib transports
// (that mix would make BrowserHeaders disagree with the actual fingerprint).
// Only when every TLS client fails do we fall back to the stdlib for all
// proxies, matching the old single-proxy behavior.
func newRequester(cfg config.Config) Requester {
	proxies := cfg.ProxyList()

	if cfg.Impersonate != "" {
		// A single direct TLS client (no proxy) still has to be built. Use an
		// explicit empty entry so the loop below covers that case too.
		targets := proxies
		if len(targets) == 0 {
			targets = []string{""}
		}

		entries := make([]*proxyRequesterEntry, 0, len(targets))
		for _, proxyURL := range targets {
			adapter, err := getTLSClient(cfg.Impersonate, cfg.RequestTimeoutSec, proxyURL)
			if err != nil {
				if proxyURL == "" {
					log.Printf("Failed to create TLS client for profile %s: %v; falling back to stdlib",
						cfg.Impersonate, err)
				} else {
					log.Printf("Failed to create TLS client for profile %s via proxy %s: %v; skipping this proxy",
						cfg.Impersonate, proxypool.Redact(proxyURL), err)
				}
				continue
			}
			entries = append(entries, &proxyRequesterEntry{proxyURL: proxyURL, requester: adapter})
		}
		if len(entries) == 0 {
			if len(proxies) > 0 {
				log.Printf("All TLS clients failed for %d configured proxies; falling back to stdlib", len(proxies))
			}
		} else {
			if len(entries) > 1 {
				log.Printf("Proxy rotation active: %d proxies (round-robin)", len(entries))
			}
			var fallback Requester
			if cfg.DirectFallback && len(proxies) > 0 {
				if adapter, err := getTLSClient(cfg.Impersonate, cfg.RequestTimeoutSec, ""); err != nil {
					log.Printf("Direct fallback unavailable for profile %s: %v", cfg.Impersonate, err)
				} else {
					fallback = adapter
					log.Printf("Direct fallback enabled: used only when all %d proxies are cooling", len(entries))
				}
			}
			return poolOrSingle(entries, cfg.Impersonate, fallback)
		}
	}

	entries := make([]*proxyRequesterEntry, 0, len(proxies))
	if len(proxies) == 0 {
		entries = append(entries, &proxyRequesterEntry{requester: newStdlibRequester(cfg, "")})
	} else {
		for _, proxyURL := range proxies {
			entries = append(entries, &proxyRequesterEntry{
				proxyURL:  proxyURL,
				requester: newStdlibRequester(cfg, proxyURL),
			})
		}
	}
	if len(entries) > 1 {
		log.Printf("Proxy rotation active: %d proxies (round-robin)", len(entries))
	}
	var fallback Requester
	if cfg.DirectFallback && len(proxies) > 0 {
		// A direct fallback must bypass even HTTPS_PROXY, otherwise an
		// environment proxy would silently keep the "direct" path proxied.
		fallback = newDirectStdlibRequester(cfg)
		log.Printf("Direct fallback enabled: used only when all %d proxies are cooling", len(entries))
	}
	return poolOrSingle(entries, "", fallback)
}

// poolOrSingle avoids the pooling indirection when only one transport exists
// and no fallback is configured, so single-proxy installs keep the exact
// previous behavior.
func poolOrSingle(entries []*proxyRequesterEntry, profile string, fallback Requester) Requester {
	if len(entries) == 1 && fallback == nil {
		return entries[0].requester
	}
	return newProxyRequesterPool(entries, profile, fallback)
}

func newStdlibRequester(cfg config.Config, proxyURL string) Requester {
	var proxy func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			proxy = http.ProxyURL(u)
		}
	} else {
		proxy = http.ProxyFromEnvironment
	}
	return newStdlibRequesterWithProxy(cfg, proxy)
}

// newDirectStdlibRequester is a true direct client: Proxy stays nil so an
// HTTPS_PROXY environment variable cannot reroute the fallback.
func newDirectStdlibRequester(cfg config.Config) Requester {
	return newStdlibRequesterWithProxy(cfg, nil)
}

func newStdlibRequesterWithProxy(cfg config.Config, proxy func(*http.Request) (*url.URL, error)) Requester {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
			Proxy:              proxy,
		},
		Timeout: time.Duration(cfg.RequestTimeoutSec) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
