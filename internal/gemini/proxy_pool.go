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
// When every proxy is cooling the pool still uses one, so a bad health signal
// can delay traffic but cannot take the service down.
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
}

func newProxyRequesterPool(entries []*proxyRequesterEntry, profile string) *proxyRequesterPool {
	cp := make([]*proxyRequesterEntry, len(entries))
	copy(cp, entries)
	return &proxyRequesterPool{
		entries: cp,
		profile: profile,
		now:     time.Now,
	}
}

// Do sends the request through the next healthy proxy in round-robin order.
func (p *proxyRequesterPool) Do(req *http.Request) (*http.Response, error) {
	entry := p.nextEntry()
	resp, err := entry.requester.Do(req)
	p.record(entry, resp, err)
	return resp, err
}

// Profile reports the TLS impersonation profile shared by every requester in
// the pool. It is empty when the pool uses the stdlib transport fallback.
func (p *proxyRequesterPool) Profile() string { return p.profile }

// Len reports the number of active proxies in the pool.
func (p *proxyRequesterPool) Len() int { return len(p.entries) }

// nextEntry applies round-robin while skipping proxies in cooldown. If every
// proxy is cooling, it falls back to the round-robin pick so traffic resumes
// rather than failing closed.
func (p *proxyRequesterPool) nextEntry() *proxyRequesterEntry {
	n := len(p.entries)
	start := int((p.next.Add(1) - 1) % uint64(n))
	now := p.now().UnixNano()
	for i := 0; i < n; i++ {
		entry := p.entries[(start+i)%n]
		if entry.cooldownTill.Load() <= now {
			return entry
		}
	}
	return p.entries[start]
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
			return poolOrSingle(entries, cfg.Impersonate)
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
	return poolOrSingle(entries, "")
}

// poolOrSingle avoids the pooling indirection when only one transport exists,
// so single-proxy installs keep the exact previous behavior.
func poolOrSingle(entries []*proxyRequesterEntry, profile string) Requester {
	if len(entries) == 1 {
		return entries[0].requester
	}
	return newProxyRequesterPool(entries, profile)
}

func newStdlibRequester(cfg config.Config, proxyURL string) Requester {
	transport := &http.Transport{
		DisableCompression: true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}

	return &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.RequestTimeoutSec) * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
