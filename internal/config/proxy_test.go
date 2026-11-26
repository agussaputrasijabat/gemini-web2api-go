package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProxyListMergesLegacyAndList(t *testing.T) {
	cfg := Config{
		Proxy: "http://legacy.example:1",
		Proxies: []string{
			"http://a.example:2",
			"http://legacy.example:1",
			"ftp://bad.example:21",
		},
	}

	want := []string{"http://legacy.example:1", "http://a.example:2"}
	if got := cfg.ProxyList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProxyList = %v, want %v", got, want)
	}
	if got := cfg.InvalidProxies(); len(got) != 1 || got[0] != "ftp://bad.example:21" {
		t.Fatalf("InvalidProxies = %v, want the ftp entry", got)
	}
}

func TestProxyListNormalizesBareHosts(t *testing.T) {
	cfg := Config{Proxies: []string{"127.0.0.1:60000", " SOCKS5://127.0.0.1:60001 "}}
	want := []string{"http://127.0.0.1:60000", "socks5://127.0.0.1:60001"}
	if got := cfg.ProxyList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProxyList = %v, want %v", got, want)
	}
}

func TestProxyListEmpty(t *testing.T) {
	cfg := Config{}
	if got := cfg.ProxyList(); len(got) != 0 {
		t.Fatalf("ProxyList = %v, want empty", got)
	}
	if got := cfg.RawProxyList(); len(got) != 0 {
		t.Fatalf("RawProxyList = %v, want empty", got)
	}
}

func TestLoadDirectFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"proxies": ["127.0.0.1:60000", "127.0.0.1:60001"],
		"direct_fallback": true
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.DirectFallback {
		t.Fatal("DirectFallback = false, want true")
	}
	want := []string{"http://127.0.0.1:60000", "http://127.0.0.1:60001"}
	if got := cfg.ProxyList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProxyList = %v, want %v", got, want)
	}
}
