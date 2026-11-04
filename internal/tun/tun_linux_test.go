//go:build linux

package tun

import "testing"

func TestLogLevel(t *testing.T) {
	for _, in := range []string{"silent", "error", "warning", "info", "debug"} {
		if got := logLevel(in); got != in {
			t.Errorf("logLevel(%q) = %q", in, got)
		}
	}
	if got := logLevel(""); got != "warning" {
		t.Errorf("empty level = %q, want warning", got)
	}
	if got := logLevel("bogus"); got != "warning" {
		t.Errorf("unknown level = %q, want warning", got)
	}
}

func TestContainsRule(t *testing.T) {
	// `ip rule list` prints marks in hex; 1987 == 0x7c3.
	hexRules := "0:\tfrom all lookup local\n100:\tfrom all fwmark 0x7c3 lookup 1987\n"
	if !containsRule(hexRules, "1987", "1987") {
		t.Fatal("containsRule should match the hex form of the mark")
	}
	decRules := "100:\tfrom all fwmark 1987 lookup 1987\n"
	if !containsRule(decRules, "1987", "1987") {
		t.Fatal("containsRule should match the decimal form")
	}
	if containsRule("0:\tfrom all lookup local\n", "1987", "1987") {
		t.Fatal("containsRule should not match when the rule is absent")
	}
}
