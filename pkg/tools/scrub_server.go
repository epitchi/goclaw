package tools

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// publicIPServices returns the caller's public IP as plain text.
// Tried in order; first successful response is used.
var publicIPServices = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
	"https://checkip.amazonaws.com",
}

// DetectServerIPs discovers the server's local and public IP addresses
// and registers them as dynamic scrub values.
// Local IPs are detected synchronously. Public IP runs in a goroutine.
func DetectServerIPs(ctx context.Context) {
	// Phase 1: Local interface IPs (synchronous, microseconds)
	localIPs := detectLocalIPs()
	if len(localIPs) > 0 {
		AddDynamicScrubValues(localIPs...)
		slog.Info("security.server_ip_scrub: local IPs registered",
			"count", len(localIPs), "ips", localIPs)
	}

	// Phase 2: Public IP (async, HTTP call)
	go func() {
		pubIP := detectPublicIP(ctx)
		if pubIP != "" {
			AddDynamicScrubValues(pubIP)
			slog.Info("security.server_ip_scrub: public IP registered", "ip", pubIP)
		} else {
			slog.Warn("security.server_ip_scrub: public IP detection failed")
		}
	}()
}

// detectLocalIPs returns non-loopback, non-link-local IPs from local interfaces.
func detectLocalIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		slog.Warn("security.server_ip_scrub: failed to list interfaces", "error", err)
		return nil
	}

	var ips []string
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}

		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			continue
		}

		ips = append(ips, ip.String())
	}
	return ips
}

// detectPublicIP tries multiple services to find the server's public IP.
func detectPublicIP(ctx context.Context) string {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        2,
			IdleConnTimeout:     10 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
		},
	}

	for _, svcURL := range publicIPServices {
		ip := tryPublicIPService(ctx, client, svcURL)
		if ip != "" {
			return ip
		}
	}
	return ""
}

// tryPublicIPService makes an HTTP GET to a service that returns the caller's IP as plain text.
func tryPublicIPService(ctx context.Context, client *http.Client, svcURL string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", svcURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "goclaw/ip-check")

	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("security.server_ip_scrub: service failed", "url", svcURL, "error", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}

	candidate := strings.TrimSpace(string(body))
	if net.ParseIP(candidate) == nil {
		return ""
	}
	return candidate
}
