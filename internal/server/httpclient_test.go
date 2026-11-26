package server

import (
	"net/http"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

func TestCreateHTTPClientSingleProxy(t *testing.T) {
	cfg := config.Config{Proxy: "http://127.0.0.1:60000"}
	client := createHTTPClient(cfg)
	if _, ok := client.Transport.(*http.Transport); !ok {
		t.Fatalf("single-proxy transport type = %T, want *http.Transport", client.Transport)
	}
}

func TestCreateHTTPClientMultipleProxies(t *testing.T) {
	cfg := config.Config{Proxies: []string{
		"http://127.0.0.1:60000",
		"socks5://127.0.0.1:60001",
	}}
	client := createHTTPClient(cfg)
	rr, ok := client.Transport.(*roundRobinTransport)
	if !ok {
		t.Fatalf("multi-proxy transport type = %T, want *roundRobinTransport", client.Transport)
	}
	if rr.pool.Len() != 2 {
		t.Fatalf("pool Len = %d, want 2", rr.pool.Len())
	}
	first, _ := rr.pool.Next()
	second, _ := rr.pool.Next()
	if first == second {
		t.Fatal("round-robin must alternate transports")
	}
}

func TestCreateHTTPClientNoProxy(t *testing.T) {
	client := createHTTPClient(config.Config{})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("no-proxy transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("no-proxy transport must keep environment proxy support")
	}
}
