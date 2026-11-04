package node

import "testing"

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
