package gemini

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

type fakeRequester struct {
	name  string
	calls atomic.Int32
	err   error
	reply func() *http.Response
}

func (f *fakeRequester) Do(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if f.reply != nil {
		return f.reply(), nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

func proxyEntries(requesters ...Requester) []*proxyRequesterEntry {
	entries := make([]*proxyRequesterEntry, 0, len(requesters))
	for i, requester := range requesters {
		entries = append(entries, &proxyRequesterEntry{
			proxyURL:  "http://proxy-" + string(rune('a'+i)) + ".example:8080",
			requester: requester,
		})
	}
	return entries
}

func TestProxyRequesterPoolRotates(t *testing.T) {
	requesters := []Requester{
		&fakeRequester{name: "a"},
		&fakeRequester{name: "b"},
		&fakeRequester{name: "c"},
	}
	pool := newProxyRequesterPool(proxyEntries(requesters...), "chrome_146", nil)

	if pool.Len() != 3 {
		t.Fatalf("Len = %d, want 3", pool.Len())
	}
	if got := pool.Profile(); got != "chrome_146" {
		t.Fatalf("Profile = %q, want chrome_146", got)
	}
	if got := ProfileOf(pool); got != "chrome_146" {
		t.Fatalf("ProfileOf(pool) = %q, want chrome_146", got)
	}

	for i := 0; i < len(requesters)*2; i++ {
		if _, err := pool.Do(&http.Request{}); err != nil {
			t.Fatalf("Do #%d error: %v", i, err)
		}
	}
	for _, requester := range requesters {
		fake := requester.(*fakeRequester)
		if got := fake.calls.Load(); got != 2 {
			t.Errorf("requester %s called %d times, want 2", fake.name, got)
		}
	}
}

func TestProxyRequesterPoolSkipsCoolingProxy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	bad := &fakeRequester{name: "bad", err: errors.New("connection refused")}
	good := &fakeRequester{name: "good"}
	pool := newProxyRequesterPool(proxyEntries(bad, good), "", nil)
	pool.now = func() time.Time { return now }

	// First request hits the bad proxy and cools it down.
	if _, err := pool.Do(&http.Request{}); err == nil {
		t.Fatal("first request must surface the bad proxy error")
	}
	if got := bad.calls.Load(); got != 1 {
		t.Fatalf("bad proxy calls = %d, want 1", got)
	}

	// While the bad proxy is cooling, every request should use the good one.
	for i := 0; i < 5; i++ {
		if _, err := pool.Do(&http.Request{}); err != nil {
			t.Fatalf("request %d error: %v", i, err)
		}
	}
	if got := bad.calls.Load(); got != 1 {
		t.Fatalf("cooling proxy was used again: calls = %d, want 1", got)
	}
	if got := good.calls.Load(); got != 5 {
		t.Fatalf("good proxy calls = %d, want 5", got)
	}

	// Once the cooldown expires the proxy re-enters rotation.
	now = now.Add(proxyTransportCooldown + time.Second)
	bad.err = nil
	if _, err := pool.Do(&http.Request{}); err != nil {
		t.Fatalf("post-cooldown request error: %v", err)
	}
	if got := bad.calls.Load(); got != 2 {
		t.Fatalf("bad proxy calls after cooldown = %d, want 2", got)
	}
}

func TestProxyRequesterPoolUsesAllWhenEveryProxyCools(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := &fakeRequester{name: "a", err: errors.New("timeout")}
	b := &fakeRequester{name: "b", err: errors.New("timeout")}
	pool := newProxyRequesterPool(proxyEntries(a, b), "", nil)
	pool.now = func() time.Time { return now }

	for i := 0; i < 4; i++ {
		_, _ = pool.Do(&http.Request{})
	}
	// Both are cooling, but the pool must keep attempting instead of failing
	// without a request.
	if got := a.calls.Load() + b.calls.Load(); got != 4 {
		t.Fatalf("pool stopped using cooling proxies: total calls = %d, want 4", got)
	}
}

func TestProxyCooldownOnRateLimitAndClearOnSuccess(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limited := &fakeRequester{
		name: "limited",
		reply: func() *http.Response {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
		},
	}
	pool := newProxyRequesterPool(proxyEntries(limited), "", nil)
	pool.now = func() time.Time { return now }

	if _, err := pool.Do(&http.Request{}); err != nil {
		t.Fatalf("rate-limit response must not be a transport error: %v", err)
	}
	if until := pool.entries[0].cooldownTill.Load(); until != now.Add(proxyRateLimitCooldown).UnixNano() {
		t.Fatalf("cooldown = %d, want %d", until, now.Add(proxyRateLimitCooldown).UnixNano())
	}

	limited.reply = nil // now return HTTP 200
	now = now.Add(proxyRateLimitCooldown + time.Second)
	if _, err := pool.Do(&http.Request{}); err != nil {
		t.Fatalf("success request error: %v", err)
	}
	if until := pool.entries[0].cooldownTill.Load(); until != 0 {
		t.Fatalf("success must clear cooldown, got %d", until)
	}
}

func TestShouldRotateOnOnlyForProxySpecificFailures(t *testing.T) {
	pool := newProxyRequesterPool(proxyEntries(
		&fakeRequester{name: "a"},
		&fakeRequester{name: "b"},
	), "", nil)
	multi := &Client{HTTP: pool}

	if !multi.shouldRotateOn(&UpstreamError{Kind: KindBlocked}) {
		t.Error("multi-proxy pool must rotate after a block")
	}
	if !multi.shouldRotateOn(&UpstreamError{Kind: KindRateLimited}) {
		t.Error("multi-proxy pool must rotate after a rate limit")
	}
	if multi.shouldRotateOn(&UpstreamError{Kind: KindAuth}) {
		t.Error("auth failures are global and must not rotate")
	}
	if multi.shouldRotateOn(&UpstreamError{Kind: KindBLEpired}) {
		t.Error("stale BL failures are global and must not rotate")
	}

	single := &Client{HTTP: &fakeRequester{name: "only"}}
	if single.shouldRotateOn(&UpstreamError{Kind: KindBlocked}) {
		t.Error("single-proxy client must not rotate after a block")
	}
	if single.proxyCount() != 1 {
		t.Errorf("single requester proxyCount = %d, want 1", single.proxyCount())
	}
}

func TestNewRequesterKeepsSingleTLSAdapter(t *testing.T) {
	cfg := config.Default()
	cfg.Impersonate = "chrome_146"
	cfg.Proxies = []string{"http://127.0.0.1:60000"}
	cfg.RequestTimeoutSec = 30

	requester := newRequester(cfg)
	if _, ok := requester.(*tlsClientAdapter); !ok {
		t.Fatalf("single proxy requester type = %T, want *tlsClientAdapter", requester)
	}
}

func TestNewRequesterBuildsPoolForMultipleProxies(t *testing.T) {
	cfg := config.Default()
	cfg.Impersonate = "chrome_146"
	cfg.Proxies = []string{"http://127.0.0.1:60000", "socks5://127.0.0.1:60001"}
	cfg.RequestTimeoutSec = 30

	requester := newRequester(cfg)
	pool, ok := requester.(*proxyRequesterPool)
	if !ok {
		t.Fatalf("multi-proxy requester type = %T, want *proxyRequesterPool", requester)
	}
	if pool.Len() != 2 {
		t.Fatalf("pool Len = %d, want 2", pool.Len())
	}
	if got := pool.Profile(); got != "chrome_146" {
		t.Fatalf("pool Profile = %q, want chrome_146", got)
	}
}

func TestProxyRequesterPoolUsesDirectFallbackOnlyWhenAllCooling(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := &fakeRequester{name: "a", err: errors.New("connection refused")}
	b := &fakeRequester{name: "b", err: errors.New("connection refused")}
	direct := &fakeRequester{name: "direct"}
	pool := newProxyRequesterPool(proxyEntries(a, b), "", direct)
	pool.now = func() time.Time { return now }

	if !pool.HasFallback() {
		t.Fatal("HasFallback = false, want true")
	}
	if pool.Len() != 2 {
		t.Fatalf("Len = %d, want 2 proxies (fallback excluded)", pool.Len())
	}

	// First two attempts hit the two proxies and cool them down.
	for i := 0; i < 2; i++ {
		if _, err := pool.Do(&http.Request{}); err == nil {
			t.Fatalf("attempt %d must surface the proxy error", i)
		}
	}
	if got := direct.calls.Load(); got != 0 {
		t.Fatalf("direct fallback used too early: calls = %d, want 0", got)
	}

	// Every proxy is cooling now: the next request goes direct.
	if _, err := pool.Do(&http.Request{}); err != nil {
		t.Fatalf("direct fallback error: %v", err)
	}
	if got := direct.calls.Load(); got != 1 {
		t.Fatalf("direct fallback calls = %d, want 1", got)
	}

	// Once the cooldown expires the proxies re-enter rotation and the pool
	// stops using the direct fallback.
	now = now.Add(proxyTransportCooldown + time.Second)
	a.err = nil
	b.err = nil
	if _, err := pool.Do(&http.Request{}); err != nil {
		t.Fatalf("post-cooldown proxy request error: %v", err)
	}
	if got := a.calls.Load() + b.calls.Load(); got != 3 {
		t.Fatalf("recovered proxy calls = %d, want 3", got)
	}
	if got := direct.calls.Load(); got != 1 {
		t.Fatalf("direct fallback kept being used after recovery: calls = %d, want 1", got)
	}
}

func TestProxyRequesterPoolWithoutFallbackKeepsUsingCoolingProxy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := &fakeRequester{name: "a", err: errors.New("timeout")}
	b := &fakeRequester{name: "b", err: errors.New("timeout")}
	pool := newProxyRequesterPool(proxyEntries(a, b), "", nil)
	pool.now = func() time.Time { return now }

	for i := 0; i < 4; i++ {
		_, _ = pool.Do(&http.Request{})
	}
	if got := a.calls.Load() + b.calls.Load(); got != 4 {
		t.Fatalf("pool stopped using cooling proxies: calls = %d, want 4", got)
	}
}

func TestEffectiveAttemptsCoversProxiesAndFallback(t *testing.T) {
	pool := newProxyRequesterPool(proxyEntries(
		&fakeRequester{name: "a"},
		&fakeRequester{name: "b"},
		&fakeRequester{name: "c"},
	), "", &fakeRequester{name: "direct"})
	client := &Client{
		Cfg:  config.Config{RetryAttempts: 1},
		HTTP: pool,
	}
	if got := client.effectiveAttempts(); got != 4 {
		t.Fatalf("effectiveAttempts = %d, want 4 (3 proxies + direct)", got)
	}

	noFallback := &Client{
		Cfg: config.Config{RetryAttempts: 1},
		HTTP: newProxyRequesterPool(proxyEntries(
			&fakeRequester{name: "a"},
			&fakeRequester{name: "b"},
		), "", nil),
	}
	if got := noFallback.effectiveAttempts(); got != 1 {
		t.Fatalf("effectiveAttempts without fallback = %d, want 1", got)
	}

	many := make([]*proxyRequesterEntry, 0, maxEffectiveAttempts+5)
	for i := 0; i < maxEffectiveAttempts+5; i++ {
		many = append(many, &proxyRequesterEntry{requester: &fakeRequester{name: "x"}})
	}
	capped := &Client{
		Cfg:  config.Config{RetryAttempts: 1},
		HTTP: newProxyRequesterPool(many, "", &fakeRequester{name: "direct"}),
	}
	if got := capped.effectiveAttempts(); got != maxEffectiveAttempts {
		t.Fatalf("effectiveAttempts cap = %d, want %d", got, maxEffectiveAttempts)
	}
}

func TestNewRequesterEnablesDirectFallback(t *testing.T) {
	cfg := config.Default()
	cfg.Impersonate = "chrome_146"
	cfg.Proxies = []string{"http://127.0.0.1:60000", "http://127.0.0.1:60001"}
	cfg.DirectFallback = true
	cfg.RequestTimeoutSec = 30

	requester := newRequester(cfg)
	pool, ok := requester.(*proxyRequesterPool)
	if !ok {
		t.Fatalf("requester type = %T, want *proxyRequesterPool", requester)
	}
	if !pool.HasFallback() {
		t.Fatal("direct fallback was not attached")
	}
}
