package node

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBackendRecord(t *testing.T) {
	b := &backend{}
	if b.healthy() || b.lastMs() != 0 || !math.IsInf(b.score(), 1) {
		t.Fatal("an unchecked server is unhealthy with an infinite score")
	}
	b.record(failResult())
	b.record(failResult())
	if b.fails != 2 || b.errTotal != 2 || b.healthy() {
		t.Fatalf("after failures: %+v", b)
	}
	ok := okResult(40)
	b.record(ok)
	if b.fails != 0 || b.okTotal != 1 || !b.lastOK.Equal(ok.at) || !b.healthy() || b.lastMs() != 40 {
		t.Fatalf("after success: %+v", b)
	}
	b.record(failResult())
	if b.healthy() || b.lastMs() != 40 {
		t.Fatal("lastMs must report the last successful check")
	}
	for range 2 * historySize {
		b.record(okResult(10))
	}
	if len(b.hist) != historySize {
		t.Fatalf("history not trimmed: %d", len(b.hist))
	}
}

func TestBackendScore(t *testing.T) {
	b := &backend{}
	for _, ms := range []int{300, 100, 200} {
		b.record(okResult(ms))
	}
	if got := b.score(); got != 200 {
		t.Fatalf("median score = %v", got)
	}
	b.record(failResult())
	if got := b.score(); got != 1200 {
		t.Fatalf("score with one failure = %v", got)
	}
	flaky := &backend{}
	flaky.record(okResult(10))
	flaky.record(failResult())
	stable := &backend{}
	stable.record(okResult(100))
	if flaky.score() <= stable.score() {
		t.Fatal("a flaky server must rank below a stable slower one")
	}
}

func TestBackendConns(t *testing.T) {
	b := &backend{conns: map[net.Conn]net.Conn{}}
	c1, u1 := net.Pipe()
	c2, u2 := net.Pipe()
	b.track(c1, u1)
	b.track(c2, u2)
	if b.connCount() != 2 || b.dialCount() != 2 {
		t.Fatalf("count %d dials %d", b.connCount(), b.dialCount())
	}
	b.untrack(c2)
	if b.connCount() != 1 || b.dialCount() != 2 {
		t.Fatalf("after untrack: count %d dials %d", b.connCount(), b.dialCount())
	}
	if n := b.closeAll(); n != 1 {
		t.Fatalf("closeAll = %d", n)
	}
	if _, err := c1.Write([]byte("x")); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("client conn still open: %v", err)
	}
	if _, err := u1.Write([]byte("x")); !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("xray conn still open: %v", err)
	}
	c2.Close()
	u2.Close()
	if b.addr() != "127.0.0.1:0" {
		t.Fatal(b.addr())
	}
}

func TestProbe(t *testing.T) {
	target := startTarget(t)
	s := startSocks(t)
	r := probe(context.Background(), s.port(), target.URL, 2*time.Second)
	if !r.ok || r.err != "" || r.at.IsZero() {
		t.Fatalf("probe = %+v", r)
	}
}

func TestProbeHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 100<<10), http.StatusBadGateway)
	}))
	defer srv.Close()
	s := startSocks(t)
	r := probe(context.Background(), s.port(), srv.URL, 2*time.Second)
	if r.ok || r.err != "HTTP 502 Bad Gateway" {
		t.Fatalf("probe = %+v", r)
	}
}

func TestProbeRedirectIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.RedirectHandler("http://unreachable.invalid/", http.StatusFound))
	defer srv.Close()
	s := startSocks(t)
	if r := probe(context.Background(), s.port(), srv.URL, 2*time.Second); !r.ok {
		t.Fatalf("probe = %+v", r)
	}
}

func TestProbeUnreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	r := probe(context.Background(), port, "http://127.0.0.1:1/", time.Second)
	if r.ok || r.err == "" || strings.Contains(r.err, `Get "`) {
		t.Fatalf("probe = %+v", r)
	}
}

func TestProbeBadURL(t *testing.T) {
	r := probe(context.Background(), 1, "http://[::1", time.Second)
	if r.ok || r.err == "" {
		t.Fatalf("probe = %+v", r)
	}
}

func TestShortErr(t *testing.T) {
	inner := errors.New("connection refused")
	nested := &url.Error{Op: "Get", URL: "u", Err: &url.Error{Op: "Get", URL: "v", Err: inner}}
	if got := shortErr(nested); got != "connection refused" {
		t.Fatalf("shortErr = %q", got)
	}
	long := errors.New(strings.Repeat("e", 300))
	if got := shortErr(long); len(got) != 120 {
		t.Fatalf("len = %d", len(got))
	}
}
