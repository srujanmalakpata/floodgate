package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/srujanmalakpata/floodgate/internal/config"
)

// ClientKey returns the identity a route is limited by: a hash of the API key
// (raw keys are never written to Redis, logs or metrics) or the client IP.
func ClientKey(r *http.Request, route *config.Route, keyCfg config.Key) string {
	if route.KeyBy == config.KeyByAPIKey {
		if k := r.Header.Get(keyCfg.Header); k != "" {
			sum := sha256.Sum256([]byte(k))
			return "key:" + hex.EncodeToString(sum[:16])
		}
	}
	return "ip:" + ClientIP(r, keyCfg.TrustedProxyHops)
}

// ClientIP returns the caller's IP. With trustedHops == 0 it is the TCP peer.
// Behind N trusted proxies that each append to X-Forwarded-For, the client is
// the Nth entry from the right; anything further left is client-controlled and
// could be spoofed to dodge the limit, so it is ignored.
func ClientIP(r *http.Request, trustedHops int) string {
	if trustedHops > 0 {
		var hops []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for _, part := range strings.Split(h, ",") {
				if p := strings.TrimSpace(part); p != "" {
					hops = append(hops, p)
				}
			}
		}
		if i := len(hops) - trustedHops; i >= 0 && i < len(hops) {
			if addr, err := netip.ParseAddr(hops[i]); err == nil {
				return addr.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	return host
}
