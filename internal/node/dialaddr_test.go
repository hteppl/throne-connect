package node

import (
	"slices"
	"testing"
)

func TestDNSBypassCIDRs(t *testing.T) {
	in := []string{
		"https+local://1.1.1.1/dns-query",
		"https://8.8.8.8/dns-query",
		"77.88.8.8",
		"tcp://9.9.9.9:53",
		"https+local://dns.example.com/dns-query", // hostname: skipped
		"2606:4700:4700::1111",                    // IPv6: skipped (IPv4-only stack)
	}
	got := dnsBypassCIDRs(in)
	want := []string{"1.1.1.1/32", "8.8.8.8/32", "77.88.8.8/32", "9.9.9.9/32"}
	if !slices.Equal(got, want) {
		t.Fatalf("dnsBypassCIDRs = %v, want %v", got, want)
	}
}

func TestSocksDialAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:10808":     "127.0.0.1:10808",
		"127.0.0.1:10808":   "127.0.0.1:10808",
		"[::]:10808":        "127.0.0.1:10808",
		"192.168.1.5:10808": "192.168.1.5:10808",
		"garbage":           "garbage",
	}
	for in, want := range cases {
		if got := socksDialAddr(in); got != want {
			t.Errorf("socksDialAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
