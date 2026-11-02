package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"throne-connect/internal/source"
)

func TestDisabledServerIsNotCheckedOrUsed(t *testing.T) {
	target := startTarget(t)
	s0, s1, s2 := startSocks(t), startSocks(t), startSocks(t)
	n, _ := unitNode(t, baseConfig(t, target.URL), s0, s1, s2)
	ctx := ctxT(t)
	serveSwitches(ctx, n)

	if name, err := n.SetDisabled(ctx, "srv1", true); err != nil || name != "srv1" {
		t.Fatalf("name %q err %v", name, err)
	}
	n.scan(ctx)
	if b := n.backends[1]; b.okTotal+b.errTotal != 0 {
		t.Fatalf("a disabled server was checked: ok %d err %d", b.okTotal, b.errTotal)
	}
	n.mu.Lock()
	cands := n.candidates(n.backends[0])
	n.mu.Unlock()
	if len(cands) != 1 || cands[0] != n.backends[2] {
		t.Fatalf("candidates %v", cands)
	}
	if _, err := n.Switch(ctx, "srv1"); err == nil || !strings.Contains(err.Error(), "srv1 is disabled") {
		t.Fatalf("switching to a disabled server: err = %v", err)
	}

	st, _ := n.Status(ctx)
	if s := st.Servers[1]; !s.Disabled || s.Healthy || s.Score != -1 {
		t.Fatalf("status %+v", s)
	}
	if got := n.loadState().Disabled; !slices.Equal(got, []string{"key-srv1"}) {
		t.Fatalf("saved disabled %v", got)
	}

	// Disabling twice is a no-op; enabling checks the server right away.
	if _, err := n.SetDisabled(ctx, "2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := n.SetDisabled(ctx, "2", false); err != nil {
		t.Fatal(err)
	}
	if b := n.backends[1]; b.disabled || b.okTotal != 1 {
		t.Fatalf("enabled server: disabled %v ok %d", b.disabled, b.okTotal)
	}
	if got := n.loadState().Disabled; len(got) != 0 {
		t.Fatalf("saved disabled %v", got)
	}
	if _, err := n.SetDisabled(ctx, "nope", true); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestDisableActiveMovesAway(t *testing.T) {
	target := startTarget(t)
	s0, s1, s2 := startSocks(t), startSocks(t), startSocks(t)
	n, buf := unitNode(t, baseConfig(t, target.URL), s0, s1, s2)
	ctx := ctxT(t)
	serveSwitches(ctx, n)
	n.backends[1].record(okResult(50))
	n.backends[2].record(okResult(20))
	trackPair(n.backends[0])

	if _, err := n.SetDisabled(ctx, "srv0", true); err != nil {
		t.Fatal(err)
	}
	if activeName(n) != "srv2" || !hasLog(buf, "reason=\"disabled srv0\"") {
		t.Fatalf("active %s", activeName(n))
	}
	if !hasLog(buf, "dropped_conns=1") {
		t.Fatal("connections through the disabled server must be dropped")
	}
	if n.loadState().ActiveKey != "key-srv2" {
		t.Fatal("new active server not saved")
	}
}

func TestDisableActiveKeptWhenNothingElseWorks(t *testing.T) {
	target := startTarget(t)
	s0, s1 := startSocks(t), startSocks(t)
	s1.fail.Store(true)
	n, _ := unitNode(t, baseConfig(t, target.URL), s0, s1)
	ctx := ctxT(t)
	serveSwitches(ctx, n)
	if _, err := n.SetDisabled(ctx, "srv0", true); err == nil || !strings.Contains(err.Error(), "no other server responds") {
		t.Fatalf("err = %v", err)
	}
	if activeName(n) != "srv0" || n.backends[0].disabled || n.disabled["key-srv0"] {
		t.Fatal("the active server must stay enabled")
	}
}

func TestDisableLastEnabled(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""), startSocks(t), startSocks(t))
	ctx := ctxT(t)
	serveSwitches(ctx, n)
	n.backends[1].disabled = true
	if _, err := n.SetDisabled(ctx, "srv0", true); err == nil || !strings.Contains(err.Error(), "only enabled server") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyKeepsDisabled(t *testing.T) {
	n := applyNode(t)
	n.disabled["key-b"] = true
	if err := n.apply([]source.Server{testServer("a"), testServer("b")}, true); err != nil {
		t.Fatal(err)
	}
	if n.backends[0].disabled || !n.backends[1].disabled {
		t.Fatal("disabled flag not applied by key")
	}

	// The active server became disabled: the next list starts on an enabled one.
	n.active.Store(n.backends[1])
	if err := n.apply([]source.Server{testServer("b"), testServer("c")}, false); err != nil {
		t.Fatal(err)
	}
	if activeName(n) != "c" {
		t.Fatalf("active %s, want c", activeName(n))
	}

	// A list with only disabled servers is used anyway.
	if err := n.apply([]source.Server{testServer("b")}, false); err != nil {
		t.Fatal(err)
	}
	if n.backends[0].disabled || activeName(n) != "b" || !n.disabled["key-b"] {
		t.Fatal("an all-disabled list must still be used, keeping the saved choice")
	}
}

func TestAPIDisable(t *testing.T) {
	n, c := apiNode(t, startSocks(t), startSocks(t))
	ctx := context.Background()
	if name, err := c.SetDisabled(ctx, "2", true); err != nil || name != "srv1" {
		t.Fatalf("name %q err %v", name, err)
	}
	n.mu.Lock()
	off := n.backends[1].disabled
	n.mu.Unlock()
	if !off {
		t.Fatal("not disabled")
	}
	if name, err := c.SetDisabled(ctx, "srv1", false); err != nil || name != "srv1" {
		t.Fatalf("name %q err %v", name, err)
	}
	if _, err := c.SetDisabled(ctx, "", true); err == nil || !strings.Contains(err.Error(), "missing ?server=") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.SetDisabled(ctx, "nope", false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}
