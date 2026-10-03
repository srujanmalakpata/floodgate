package gateway

import (
	"net/http/httptest"
	"testing"

	"github.com/srujanmalakpata/floodgate/internal/config"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		xff    []string
		hops   int
		want   string
	}{
		{"tcp peer", "203.0.113.7:5555", nil, 0, "203.0.113.7"},
		{"xff ignored without trusted hops", "10.0.0.1:1", []string{"198.51.100.1"}, 0, "10.0.0.1"},
		{"one trusted proxy", "10.0.0.1:1", []string{"198.51.100.1"}, 1, "198.51.100.1"},
		{"spoofed entries to the left are ignored", "10.0.0.1:1", []string{"6.6.6.6, 198.51.100.1"}, 1, "198.51.100.1"},
		{"two trusted proxies", "10.0.0.2:1", []string{"6.6.6.6, 198.51.100.1, 10.0.0.1"}, 2, "198.51.100.1"},
		{"multiple header lines", "10.0.0.2:1", []string{"198.51.100.1", "10.0.0.1"}, 2, "198.51.100.1"},
		{"fewer entries than hops falls back to peer", "10.0.0.1:1", []string{"198.51.100.1"}, 3, "10.0.0.1"},
		{"garbage falls back to peer", "10.0.0.1:1", []string{"not-an-ip"}, 1, "10.0.0.1"},
		{"ipv6 peer", "[2001:db8::1]:443", nil, 0, "2001:db8::1"},
		{"ipv4-mapped ipv6 is normalised", "[::ffff:192.0.2.1]:80", nil, 0, "192.0.2.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, tt.hops); got != tt.want {
				t.Fatalf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientKey(t *testing.T) {
	keyCfg := config.Key{Header: "X-API-Key"}
	byKey := &config.Route{KeyBy: config.KeyByAPIKey}
	byIP := &config.Route{KeyBy: config.KeyByIP}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	if got := ClientKey(r, byKey, keyCfg); got != "ip:192.0.2.10" {
		t.Fatalf("no API key should fall back to IP, got %q", got)
	}

	r.Header.Set("X-API-Key", "secret-key-123")
	got := ClientKey(r, byKey, keyCfg)
	if len(got) != len("key:")+32 || got[:4] != "key:" {
		t.Fatalf("API key identity = %q, want key:<32 hex chars>", got)
	}
	if got == "key:secret-key-123" || ClientKey(r, byKey, keyCfg) != got {
		t.Fatal("API key must be hashed deterministically, never stored raw")
	}
	if got := ClientKey(r, byIP, keyCfg); got != "ip:192.0.2.10" {
		t.Fatalf("key_by ip must ignore the API key, got %q", got)
	}
}
