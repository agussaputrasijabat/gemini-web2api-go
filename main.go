package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
	proxypool "github.com/ikhsan3adi/gemini-web2api/internal/proxy"
	"github.com/ikhsan3adi/gemini-web2api/internal/server"
)

var Version = "dev"

// stringListFlag accepts a repeatable flag and also splits comma-separated
// values, so both `--proxy a --proxy b` and `--proxy a,b` work.
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, splitProxyList(value)...)
	return nil
}

func splitProxyList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func resolveVersion() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return Version
}

func main() {
	currentVersion := resolveVersion()

	portFlag := flag.Int("port", 0, "Server port")
	configFlag := flag.String("config", "", "Config file path")
	cookieFlag := flag.String("cookie-file", "", "Cookie file path")
	var proxyFlags stringListFlag
	flag.Var(&proxyFlags, "proxy", "Proxy URL (repeatable or comma-separated; requests rotate round-robin)")
	flag.Var(&proxyFlags, "proxies", "Alias for --proxy (repeatable or comma-separated)")
	impersonateFlag := flag.String("impersonate", "", "TLS impersonation profile")
	versionFlag := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gemini-web2api %s\n", currentVersion)
		os.Exit(0)
	}

	configPath := *configFlag
	if configPath == "" {
		configPath = os.Getenv("GEMINI_WEB2API_CONFIG")
	}
	if configPath == "" {
		configPath = config.Find()
	}

	cfg, err := config.Load(configPath)
	if err != nil && configPath != "" {
		log.Printf("Warning: failed to load config from %s: %v", configPath, err)
	}

	if *portFlag != 0 {
		cfg.Port = *portFlag
	}
	if *cookieFlag != "" {
		cfg.CookieFile = *cookieFlag
	}
	// Proxy precedence: CLI flags > GEMINI_WEB2API_PROXIES > config.json.
	if len(proxyFlags) == 0 {
		if env := os.Getenv("GEMINI_WEB2API_PROXIES"); strings.TrimSpace(env) != "" {
			cfg.Proxy = ""
			cfg.Proxies = splitProxyList(env)
		}
	} else {
		cfg.Proxy = proxyFlags[0]
		cfg.Proxies = append([]string(nil), proxyFlags[1:]...)
	}
	if *impersonateFlag != "" {
		cfg.Impersonate = *impersonateFlag
	}

	if invalid := cfg.InvalidProxies(); len(invalid) > 0 {
		redacted := make([]string, 0, len(invalid))
		for _, entry := range invalid {
			redacted = append(redacted, proxypool.Redact(entry))
		}
		log.Printf("Ignoring %d invalid proxy entries: %s",
			len(invalid), strings.Join(redacted, ", "))
	}

	app := server.New(cfg, currentVersion)

	// Blocking BL refresh at startup - never fail boot on error.
	if newBL, changed, blErr := app.Gem.UpdateBLIfNeeded(); blErr != nil {
		log.Printf("BL auto-update failed (non-fatal): %v", blErr)
	} else if changed {
		log.Printf("BL auto-updated: %s -> %s", cfg.GeminiBL, newBL)
	}

	app.Gem.EnsureModelsDiscovered()
	if d := app.Gem.DiscoveredModels(); len(d) > 0 {
		log.Printf("Discovered %d account models via otAQ7b", len(d))
	}

	modelKeys := make([]string, 0, len(models.MODELS))
	for k := range models.MODELS {
		modelKeys = append(modelKeys, k)
	}

	cookieStatus := "none (anonymous)"
	if cfg.CookieFile != "" {
		cookieStatus = fmt.Sprintf("yes (%s)", cfg.CookieFile)
	}

	proxyList := cfg.ProxyList()
	proxyStatus := "system env (no explicit proxy)"
	switch len(proxyList) {
	case 1:
		proxyStatus = proxypool.Redact(proxyList[0])
	case 0:
	default:
		redacted := make([]string, 0, len(proxyList))
		for _, proxyURL := range proxyList {
			redacted = append(redacted, proxypool.Redact(proxyURL))
		}
		proxyStatus = fmt.Sprintf("%d proxies round-robin (%s)",
			len(proxyList), strings.Join(redacted, ", "))
	}

	fallbackStatus := "no"
	if cfg.DirectFallback && len(proxyList) > 0 {
		fallbackStatus = "yes (only when all proxies are cooling)"
	}

	impersonateStatus := cfg.Impersonate
	if impersonateStatus == "" {
		impersonateStatus = "none (stdlib)"
	}

	temporaryStatus := "no"
	if cfg.TemporaryChats {
		temporaryStatus = "yes"
	}

	fmt.Printf("gemini-web2api %s\n", currentVersion)
	fmt.Printf("  Listening:   http://%s:%d\n", cfg.Host, cfg.Port)
	fmt.Printf("  Base URL:    http://localhost:%d/v1\n", cfg.Port)
	fmt.Printf("  Models:      %s\n", strings.Join(modelKeys, ", "))
	fmt.Printf("  Cookie:      %s\n", cookieStatus)
	fmt.Printf("  Proxy:       %s\n", proxyStatus)
	fmt.Printf("  Fallback:    %s\n", fallbackStatus)
	fmt.Printf("  Impersonate: %s\n", impersonateStatus)
	fmt.Printf("  Temporary:   %s\n", temporaryStatus)
	fmt.Println()

	if cfg.Impersonate == "" {
		log.Printf("WARNING: impersonate is empty - upstream requests use the Go stdlib TLS/HTTP2 "+
			"fingerprint while sending a browser User-Agent. That UA/TLS mismatch raises the risk "+
			"score and is a common cause of sorry/index (IP flag) blocks. Set --impersonate %s "+
			"(valid profiles: %s).",
			gemini.DefaultProfileName, strings.Join(gemini.KnownProfiles(), ", "))
	} else {
		if !gemini.IsKnownProfile(cfg.Impersonate) {
			log.Printf("WARNING: impersonate profile %q is not recognized; requests will use %s "+
				"instead. Valid profiles: %s",
				cfg.Impersonate, gemini.DefaultProfileName, strings.Join(gemini.KnownProfiles(), ", "))
		}
		log.Printf("TLS impersonation active: profile=%s proxy=%s", cfg.Impersonate, proxyStatus)
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
	}

	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-stopCtx.Done()
	log.Println("Shutting down server gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced shutdown: %v", err)
	}

	log.Println("Stopped.")
}
