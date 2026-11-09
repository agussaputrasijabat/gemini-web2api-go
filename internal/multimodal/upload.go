package multimodal

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
)

func UploadImage(client gemini.Requester, tokens PageTokens, imgBytes []byte, mime string, cookieCache *gemini.CookieCache, authUser string) (string, error) {
	if mime == "" {
		mime = "image/png"
	}

	// One UA source of truth: derive it from the transport's impersonation
	// profile so an upload never claims a browser version the TLS fingerprint
	// does not have.
	userAgent := gemini.UserAgent(gemini.ProfileOf(client))

	pushID := tokens.PushID
	if pushID == "" {
		pushID = DefaultPushID
	}
	pctx := tokens.Pctx
	if pctx == "" {
		pctx = DefaultPctx
	}

	cookieInfo, _ := cookieCache.Load()

	// Step 1: Initiate resumable upload
	startHeaders := make(http.Header)
	startHeaders.Set("Push-ID", pushID)
	startHeaders.Set("X-Tenant-Id", "bard-storage")
	startHeaders.Set("X-Client-Pctx", pctx)
	startHeaders.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprintf("%d", len(imgBytes)))
	startHeaders.Set("X-Goog-Upload-Header-Content-Type", mime)
	startHeaders.Set("X-Goog-Upload-Protocol", "resumable")
	startHeaders.Set("X-Goog-Upload-Command", "start")
	startHeaders.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	startHeaders.Set("User-Agent", userAgent)

	if cookieInfo.Cookie != "" {
		startHeaders.Set("Cookie", cookieInfo.Cookie)
	}
	if cookieInfo.SAPISID != "" {
		startHeaders.Set("Authorization", gemini.SAPISIDHash(cookieInfo.SAPISID))
	}

	startURL := "https://content-push.googleapis.com/upload/"
	req1, err := http.NewRequest("POST", startURL, bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	req1.Header = startHeaders

	resp1, err := client.Do(req1)
	if err != nil {
		return "", fmt.Errorf("upload step 1 failed: %w", err)
	}
	defer func() { _ = resp1.Body.Close() }()

	uploadURL := resp1.Header.Get("X-Goog-Upload-URL")
	if uploadURL == "" {
		uploadURL = resp1.Header.Get("x-goog-upload-url")
	}
	if uploadURL == "" {
		return "", fmt.Errorf("no upload URL in response headers")
	}

	// Step 2: Upload file data + finalize
	uploadHeaders := make(http.Header)
	uploadHeaders.Set("X-Goog-Upload-Command", "upload, finalize")
	uploadHeaders.Set("X-Goog-Upload-Offset", "0")
	uploadHeaders.Set("Content-Type", "application/octet-stream")
	uploadHeaders.Set("User-Agent", userAgent)

	req2, err := http.NewRequest("POST", uploadURL, bytes.NewReader(imgBytes))
	if err != nil {
		return "", err
	}
	req2.Header = uploadHeaders

	resp2, err := client.Do(req2)
	if err != nil {
		return "", fmt.Errorf("upload step 2 failed: %w", err)
	}
	defer func() { _ = resp2.Body.Close() }()

	bodyBytes, err := io.ReadAll(resp2.Body)
	if err != nil {
		return "", err
	}

	fileRef := strings.TrimSpace(string(bodyBytes))
	if fileRef == "" || !strings.HasPrefix(fileRef, "/") {
		return "", fmt.Errorf("invalid file reference: %s", fileRef)
	}

	return fileRef, nil
}

// maxImageBytes caps how much of an unauthenticated remote response is read.
// Images are compressed before upload anyway; anything beyond this is abuse.
const maxImageBytes = 32 << 20 // 32 MiB

// FetchImageBytes downloads an image referenced by URL.
//
// SECURITY: this function makes a request to a URL chosen by the caller, so it
// is an SSRF sink. The guard rejects non-http(s) schemes and any host that
// resolves to a loopback, private, link-local, or otherwise non-public address.
// Redirects are not followed, because a public URL can redirect to 169.254.169.254.
func FetchImageBytes(client gemini.Requester, imageURL string) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(imageURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid image URL scheme, only http/https allowed")
	}
	if err := checkPublicHost(parsed.Hostname()); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("GET", parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", gemini.UserAgent(gemini.ProfileOf(client)))

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Image fetch failed: %v", err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// A redirect can land on an internal address; refuse rather than chase it.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("image URL redirected (HTTP %d); redirects are not followed", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image fetch returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("image exceeds %d byte limit", maxImageBytes)
	}
	if mime := DetectImageMime(data); mime == "" {
		return nil, fmt.Errorf("response is not a recognised image format")
	}
	return data, nil
}

// checkPublicHost resolves a hostname and rejects non-public address ranges.
// Literal IPs are checked directly; names go through the resolver so DNS
// rebinding to a private address is caught before the request is made.
func checkPublicHost(host string) error {
	if host == "" {
		return fmt.Errorf("image URL has no host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return fmt.Errorf("image URL host %q is not allowed", host)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("image URL host %q resolves to a non-public address", host)
		}
		return nil
	}

	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve image URL host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("image URL host %q resolved to no addresses", host)
	}
	for _, ip := range addrs {
		if !isPublicIP(ip) {
			return fmt.Errorf("image URL host %q resolves to a non-public address", host)
		}
	}
	return nil
}

// isPublicIP reports whether an address is routable on the public internet.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 0.0.0.0/8, 100.64.0.0/10 (CGNAT), 192.0.0.0/24, 198.18.0.0/15,
		// 240.0.0.0/4 (reserved) are not reachable public destinations.
		switch {
		case v4[0] == 0:
			return false
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			return false
		case v4[0] >= 240:
			return false
		}
	}
	return true
}
