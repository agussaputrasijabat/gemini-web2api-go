package config

import (
	"strings"

	proxypool "github.com/ikhsan3adi/gemini-web2api/internal/proxy"
)

// RawProxyList merges the legacy single proxy with the newer proxies list.
// The legacy field is kept first so existing configs keep their original
// routing order after upgrading.
func (c Config) RawProxyList() []string {
	out := make([]string, 0, len(c.Proxies)+1)
	if strings.TrimSpace(c.Proxy) != "" {
		out = append(out, c.Proxy)
	}
	out = append(out, c.Proxies...)
	return out
}

// ProxyList returns the validated, de-duplicated proxy rotation list. An empty
// result means "no explicit proxy", which lets the transports fall back to the
// system proxy environment.
func (c Config) ProxyList() []string {
	valid, _ := proxypool.NormalizeAll(c.RawProxyList())
	return valid
}

// InvalidProxies returns the configured entries that failed validation. It is
// used only for startup warnings; the values are never used for requests.
func (c Config) InvalidProxies() []string {
	_, invalid := proxypool.NormalizeAll(c.RawProxyList())
	return invalid
}
