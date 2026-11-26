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
	pool := newProxyRequesterPool(proxyEntries(requesters...), "chrome_146")

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
	pool := newProxyRequesterPool(proxyEntries(bad, good), "")
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
	pool := newProxyRequesterPool(proxyEntries(a, b), "")
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
	pool := newProxyRequesterPool(proxyEntries(limited), "")
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
	), "")
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
