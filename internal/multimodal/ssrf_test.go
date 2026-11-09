package multimodal

import (
	"net/http"
	"strings"
	"testing"
)

type mockRequester struct {
	resp *http.Response
	err  error
}

func (m *mockRequester) Do(req *http.Request) (*http.Response, error) {
	return m.resp, m.err
}

func TestFetchImageBytesSSRFReject(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"file scheme", "file:///etc/passwd", true},
		{"ftp scheme", "ftp://example.com/img.png", true},
		{"javascript scheme", "javascript:alert(1)", true},
		{"http passes scheme check", "http://example.com/img.png", false},
		{"https passes scheme check", "https://example.com/img.png", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FetchImageBytes(&mockRequester{err: http.ErrServerClosed}, tt.url)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "invalid image URL scheme") {
					t.Errorf("FetchImageBytes(%q) err=%v, want scheme error", tt.url, err)
				}
			} else {
				if err != nil && strings.Contains(err.Error(), "invalid image URL scheme") {
					t.Errorf("FetchImageBytes(%q) should NOT reject, got: %v", tt.url, err)
				}
			}
		})
	}
}

func TestCheckPublicHostRejectsInternal(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"127.1.2.3",
		"::1",
		"0.0.0.0",
		"10.1.2.3",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254", // cloud metadata endpoint
		"100.64.0.1",      // CGNAT
		"192.0.0.5",
		"198.18.0.1",
		"240.0.0.1",
		"localhost",
	}
	for _, host := range blocked {
		if err := checkPublicHost(host); err == nil {
			t.Errorf("checkPublicHost(%q) = nil, want rejection", host)
		}
	}
}

func TestCheckPublicHostAcceptsPublic(t *testing.T) {
	// Literal public IPs only: no DNS lookups in tests.
	for _, host := range []string{"8.8.8.8", "1.1.1.1", "203.0.113.5"} {
		if err := checkPublicHost(host); err != nil {
			t.Errorf("checkPublicHost(%q) = %v, want nil", host, err)
		}
	}
}

func TestFetchImageBytesRejectsPrivateHostBeforeDial(t *testing.T) {
	// The requester must not even be consulted: the guard rejects first.
	for _, u := range []string{
		"http://127.0.0.1:8081/secret.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost/x.png",
	} {
		_, err := FetchImageBytes(&mockRequester{err: http.ErrServerClosed}, u)
		if err == nil || !strings.Contains(err.Error(), "not allowed") &&
			!strings.Contains(err.Error(), "non-public") {
			t.Errorf("FetchImageBytes(%q) err=%v, want SSRF rejection", u, err)
		}
	}
}
