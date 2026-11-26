package server

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	proxypool "github.com/ikhsan3adi/gemini-web2api/internal/proxy"
)

// roundRobinTransport rotates complete http.Transports, one per proxy. Each
// transport keeps its own connection pool, so a request is never migrated to a
// different proxy in the middle of a connection.
type roundRobinTransport struct {
	pool *proxypool.Pool[*http.Transport]
}

func (t *roundRobinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport, ok := t.pool.Next()
	if !ok {
		return nil, fmt.Errorf("no proxy transports configured")
	}
	return transport.RoundTrip(req)
}

// createHTTPClient builds the auxiliary client used for image downloads and
// uploads. It follows the same proxy list and rotation order as the Gemini
// client, so every outbound path respects the multi-proxy configuration.
func createHTTPClient(cfg config.Config) *http.Client {
	proxies := cfg.ProxyList()
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	if len(proxies) <= 1 {
		proxyURL := ""
		if len(proxies) == 1 {
			proxyURL = proxies[0]
		}
		client.Transport = newProxyTransport(proxyURL)
		return client
	}

	transports := make([]*http.Transport, 0, len(proxies))
	for _, proxyURL := range proxies {
		transports = append(transports, newProxyTransport(proxyURL))
	}
	client.Transport = &roundRobinTransport{pool: proxypool.NewPool(transports)}
	return client
}

func newProxyTransport(proxyURL string) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		} else {
			transport.Proxy = http.ProxyFromEnvironment
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return transport
}
