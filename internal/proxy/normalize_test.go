package proxy

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"http", "http://127.0.0.1:60000", "http://127.0.0.1:60000", false},
		{"bare host gets http", "127.0.0.1:60000", "http://127.0.0.1:60000", false},
		{"trims space", "  http://127.0.0.1:60000  ", "http://127.0.0.1:60000", false},
		{"scheme and host lowercase", "SOCKS5://User:Pass@Proxy.Example:1080", "socks5://User:Pass@proxy.example:1080", false},
		{"socks5h", "socks5h://proxy.example:1080", "socks5h://proxy.example:1080", false},
		{"https with credentials", "https://user:secret@proxy.example:443", "https://user:secret@proxy.example:443", false},
		{"empty", "", "", true},
		{"unsupported scheme", "ftp://proxy.example:21", "", true},
		{"missing host", "http://", "", true},
		{"garbage", "://not a url", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Normalize(%q) = %q, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q) error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeAllDeduplicatesAndKeepsOrder(t *testing.T) {
	valid, invalid := NormalizeAll([]string{
		"http://a.example:1",
		" http://a.example:1 ",
		"b.example:2",
		"ftp://bad.example:21",
		"",
		"http://Proxy.Example:3",
		"http://proxy.example:3",
		"socks5://c.example:4",
	})

	wantValid := []string{
		"http://a.example:1",
		"http://b.example:2",
		"http://proxy.example:3",
		"socks5://c.example:4",
	}
	if len(valid) != len(wantValid) {
		t.Fatalf("valid = %v, want %v", valid, wantValid)
	}
	for i := range wantValid {
		if valid[i] != wantValid[i] {
			t.Fatalf("valid[%d] = %q, want %q", i, valid[i], wantValid[i])
		}
	}
	if len(invalid) != 1 || invalid[0] != "ftp://bad.example:21" {
		t.Fatalf("invalid = %v, want only the ftp entry", invalid)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"http://user:secret@proxy.example:8080": "http://***@proxy.example:8080",
		"socks5://token@proxy.example:1080":     "socks5://***@proxy.example:1080",
		"http://proxy.example:8080":             "http://proxy.example:8080",
		"not a url":                             "<invalid proxy>",
	}
	for raw, want := range cases {
		if got := Redact(raw); got != want {
			t.Errorf("Redact(%q) = %q, want %q", raw, got, want)
		}
	}
}
