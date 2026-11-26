package proxy

import (
	"fmt"
	"net/url"
	"strings"
)

var supportedSchemes = map[string]struct{}{
	"http":    {},
	"https":   {},
	"socks5":  {},
	"socks5h": {},
}

// Normalize validates one proxy URL and returns a canonical form.
//
// A missing scheme is treated as http:// because that is what most local proxy
// clients (including the common 127.0.0.1:port setup) expose. Unsupported
// schemes are rejected instead of guessed, so a typo such as ftp:// fails
// loudly during configuration rather than at request time.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty proxy URL")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if _, ok := supportedSchemes[u.Scheme]; !ok {
		return "", fmt.Errorf("unsupported proxy scheme %q (supported: http, https, socks5, socks5h)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxy URL is missing a host")
	}
	// Hostnames are case-insensitive; normalizing them makes duplicate
	// detection reliable without touching credentials or paths.
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

// NormalizeAll validates and de-duplicates a proxy list, preserving order.
// Valid entries are returned in canonical form; rejected entries are returned
// separately so callers can warn without leaking credentials.
func NormalizeAll(raw []string) (valid []string, invalid []string) {
	seen := make(map[string]struct{}, len(raw))
	valid = make([]string, 0, len(raw))
	invalid = make([]string, 0)
	for _, entry := range raw {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		normalized, err := Normalize(entry)
		if err != nil {
			invalid = append(invalid, entry)
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		valid = append(valid, normalized)
	}
	return valid, invalid
}

// Redact removes credentials from a proxy URL before it is written to logs.
// Operators can still identify the endpoint by scheme, host, and port.
func Redact(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "<invalid proxy>"
	}
	hasUser := u.User != nil
	u.User = nil
	out := u.String()
	if hasUser {
		prefix := u.Scheme + "://"
		if strings.HasPrefix(out, prefix) {
			out = prefix + "***@" + strings.TrimPrefix(out, prefix)
		}
	}
	return out
}
