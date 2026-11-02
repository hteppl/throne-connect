package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatus(t *testing.T) {
	cfg := baseConfig(t, "http://unused/")
	n, _ := unitNode(t, cfg, startSocks(t), startSocks(t), startSocks(t))
	n.backends[0].record(okResult(50))
	n.backends[1].record(okResult(80))
	n.backends[1].record(failResult())
	n.backends[0].track(net.Pipe())

	st, err := n.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Starting || st.ActiveSince != "" || st.Active != "srv0" || st.Listen != cfg.Listen {
		t.Fatalf("status: %+v", st)
	}
	if len(st.Servers) != 3 {
		t.Fatalf("servers: %+v", st.Servers)
	}
	a, b, c := st.Servers[0], st.Servers[1], st.Servers[2]
	if a.Index != 1 || !a.Active || !a.Healthy || a.LastMs != 50 || a.Score != 50 || a.Conns != 1 || a.LastErr != "" || a.Group != "main" {
		t.Fatalf("server a: %+v", a)
	}
	if b.Active || b.Healthy || b.Fails != 1 || b.OK != 1 || b.Errors != 1 || b.LastErr != "boom" || b.Score != 1080 {
		t.Fatalf("server b: %+v", b)
	}
	if c.Score != -1 || c.Healthy {
		t.Fatalf("unchecked server c: %+v", c)
	}

	n.switchedAt = time.Now()
	st, _ = n.Status(context.Background())
	if st.Starting || st.ActiveSince == "" {
		t.Fatalf("after the first switch: %+v", st)
	}
}

func TestStatusNoActive(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""))
	st, _ := n.Status(context.Background())
	if st.Active != "-" || len(st.Servers) != 0 {
		t.Fatalf("status: %+v", st)
	}
}

func TestLogs(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""))
	n.log.Info("hello")
	lines, next, err := n.Logs(context.Background(), 0)
	if err != nil || len(lines) != 1 || lines[0].Text == "" || next == 0 {
		t.Fatalf("lines %+v next %d err %v", lines, next, err)
	}
	if lines, again, _ := n.Logs(context.Background(), next); len(lines) != 0 || again != next {
		t.Fatalf("no new lines expected: %+v", lines)
	}

	bare := New(baseConfig(t, ""), nil, slog.New(slog.DiscardHandler), nil)
	lines, next, err = bare.Logs(context.Background(), 7)
	if lines != nil || next != 7 || err != nil {
		t.Fatalf("without a buffer: %v %d %v", lines, next, err)
	}
}

func TestFind(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""), startSocks(t), startSocks(t), startSocks(t))
	n.backends[0].srv.Name = "Amsterdam 1"
	n.backends[1].srv.Name = "Amsterdam 2"
	n.backends[2].srv.Name = "Berlin"
	cases := map[string]*backend{
		"1":           n.backends[0],
		"3":           n.backends[2],
		"0":           nil,
		"4":           nil,
		"Amsterdam 2": n.backends[1],
		"key-srv2":    n.backends[2],
		"berl":        n.backends[2],
		"amster":      nil, // ambiguous
		"paris":       nil,
	}
	for target, want := range cases {
		if got := n.find(target); got != want {
			t.Errorf("find(%q) = %v, want %v", target, nameOf(got), nameOf(want))
		}
	}
}

// serveSwitches handles manual requests the way the Run loop does.
func serveSwitches(ctx context.Context, n *Node) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-n.reqCh:
				req.reply <- req.run(ctx)
			}
		}
	}()
}

func TestSwitch(t *testing.T) {
	target := startTarget(t)
	cfg := baseConfig(t, target.URL)
	dead := startSocks(t)
	dead.fail.Store(true)
	n, buf := unitNode(t, cfg, startSocks(t), startSocks(t), dead)
	ctx := ctxT(t)
	serveSwitches(ctx, n)

	if _, err := n.Switch(ctx, "nope"); err == nil || !strings.Contains(err.Error(), `"nope" not found`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := n.Switch(ctx, "3"); err == nil || !strings.Contains(err.Error(), "srv2 is not responding") {
		t.Fatalf("err = %v", err)
	}
	if nameOf(n.active.Load()) != "srv0" {
		t.Fatal("a failed switch must keep the active server")
	}
	active, err := n.Switch(ctx, "2")
	if err != nil || active != "srv1" {
		t.Fatalf("active %q, err %v", active, err)
	}
	n.mu.Lock()
	pinned, failovers := n.pinned, n.failovers
	n.mu.Unlock()
	if !pinned || failovers != 1 {
		t.Fatalf("pinned %v failovers %d", pinned, failovers)
	}
	if !hasLog(buf, "reason=manual") {
		t.Fatal("manual switch not logged")
	}
}

func TestSwitchBusy(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := n.Switch(ctx, "1"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v", err)
	}
}

func apiNode(t *testing.T, servers ...*socksServer) (*Node, *Client) {
	target := startTarget(t)
	cfg := baseConfig(t, target.URL)
	cfg.APIListen = freeAddr(t)
	n, _ := unitNode(t, cfg, servers...)
	ctx := ctxT(t)
	serveSwitches(ctx, n)
	if err := n.startAPI(ctx); err != nil {
		t.Fatal(err)
	}
	return n, NewClient(cfg.APIListen)
}

func TestAPI(t *testing.T) {
	dead := startSocks(t)
	dead.fail.Store(true)
	n, c := apiNode(t, startSocks(t), startSocks(t), dead)
	n.backends[0].record(okResult(12))
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil || st.Active != "srv0" || len(st.Servers) != 3 || st.Servers[0].LastMs != 12 {
		t.Fatalf("status %+v, err %v", st, err)
	}

	lines, next, err := c.Logs(ctx, 0)
	if err != nil || len(lines) == 0 || next == 0 {
		t.Fatalf("logs %v next %d err %v", lines, next, err)
	}
	if !strings.Contains(lines[len(lines)-1].Text, "API") {
		t.Fatalf("last line: %q", lines[len(lines)-1].Text)
	}

	if _, err := c.Switch(ctx, "paris"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
	if active, err := c.Switch(ctx, "srv1"); err != nil || active != "srv1" {
		t.Fatalf("active %q, err %v", active, err)
	}
	if _, err := c.Switch(ctx, "3"); err == nil || !strings.Contains(err.Error(), "not responding") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Switch(ctx, ""); err == nil || !strings.Contains(err.Error(), "missing ?to=") {
		t.Fatalf("err = %v", err)
	}

	resp, err := http.Get(c.base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	resp, err = http.Get(c.base + "/switch?to=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /switch: %s", resp.Status)
	}
}

func TestStartAPIListenFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := baseConfig(t, "")
	cfg.APIListen = ln.Addr().String()
	n, _ := unitNode(t, cfg)
	if err := n.startAPI(ctxT(t)); err == nil {
		t.Fatal("expected a listen error")
	}
}

func TestAPIStopsWithContext(t *testing.T) {
	cfg := baseConfig(t, "")
	cfg.APIListen = freeAddr(t)
	n, buf := unitNode(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.startAPI(ctx); err != nil {
		t.Fatal(err)
	}
	c := NewClient(cfg.APIListen)
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	eventually(t, 5*time.Second, "API closed", func() bool {
		_, err := c.Status(context.Background())
		return err != nil
	})
	if hasLog(buf, "API stopped") {
		t.Fatal("a clean shutdown must not be logged as a failure")
	}
}

func TestClientErrors(t *testing.T) {
	ctx := context.Background()

	refused := NewClient(freeAddr(t))
	if _, err := refused.Status(ctx); err == nil || !strings.Contains(err.Error(), "is the node running?") {
		t.Fatalf("err = %v", err)
	}
	if lines, next, err := refused.Logs(ctx, 5); err == nil || lines != nil || next != 5 {
		t.Fatalf("logs %v %d %v", lines, next, err)
	}
	if _, err := refused.Switch(ctx, "x"); err == nil {
		t.Fatal("expected an error")
	}

	if _, err := NewClient("bad host\x7f").Status(ctx); err == nil {
		t.Fatal("expected a request error")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "not json")
		case "/logs":
			io.WriteString(w, "{broken")
		case "/switch":
			hj, _ := w.(http.Hijacker)
			conn, rw, _ := hj.Hijack()
			rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
			rw.Flush()
			conn.Close()
		}
	}))
	defer srv.Close()
	c := NewClient(strings.TrimPrefix(srv.URL, "http://"))
	if _, err := c.Status(ctx); err == nil || err.Error() != "API: 500 Internal Server Error" {
		t.Fatalf("err = %v", err)
	}
	var syn *json.SyntaxError
	if _, _, err := c.Logs(ctx, 0); !errors.As(err, &syn) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Switch(ctx, "1"); err == nil {
		t.Fatal("expected a truncated body error")
	}
}

func TestServeAndHandle(t *testing.T) {
	echo := startEcho(t)
	cfg := baseConfig(t, "")
	s := startSocks(t)
	n, _ := unitNode(t, cfg, s)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.serve(ctx, ln); close(done) }()

	c := socksDial(t, cfg.Listen, echo)
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo %q, %v", line, err)
	}
	b := n.backends[0]
	if b.connCount() != 1 || b.dialCount() != 1 {
		t.Fatalf("conns %d dials %d", b.connCount(), b.dialCount())
	}
	c.Close()
	eventually(t, 5*time.Second, "connection untracked", func() bool { return b.connCount() == 0 })

	cancel()
	ln.Close()
	<-done
}

func TestHandleNoActive(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""))
	a, b := net.Pipe()
	go n.handle(a)
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("expected the connection to be closed, got %v", err)
	}
}

func TestHandleXrayDown(t *testing.T) {
	n, buf := unitNode(t, baseConfig(t, ""))
	n.active.Store(&backend{port: freePort(t), conns: map[net.Conn]net.Conn{}})
	a, b := net.Pipe()
	go n.handle(a)
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("expected the connection to be closed, got %v", err)
	}
	eventually(t, time.Second, "logged", func() bool { return hasLog(buf, "xray unreachable") })
}

func freePort(t *testing.T) int {
	_, p, _ := net.SplitHostPort(freeAddr(t))
	var port int
	for _, ch := range p {
		port = port*10 + int(ch-'0')
	}
	return port
}

// flakyListener fails Accept once with a transient error, then reports closed.
type flakyListener struct {
	net.Listener
	calls int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.calls++
	if l.calls == 1 {
		return nil, errors.New("too many open files")
	}
	return nil, net.ErrClosed
}

func TestServeRetriesAcceptErrors(t *testing.T) {
	n, buf := unitNode(t, baseConfig(t, ""))
	ln := &flakyListener{}
	n.serve(context.Background(), ln)
	if ln.calls != 2 || !hasLog(buf, "too many open files") {
		t.Fatalf("calls %d", ln.calls)
	}
}

type errListener struct{ net.Listener }

func (errListener) Accept() (net.Conn, error) { return nil, errors.New("gone") }

func TestServeStopsWhenCancelled(t *testing.T) {
	n, buf := unitNode(t, baseConfig(t, ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.serve(ctx, errListener{})
	if hasLog(buf, "accept") {
		t.Fatal("errors after cancel must not be logged")
	}
}

func TestPipeNonTCP(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	done := make(chan struct{})
	go func() { pipe(a2, b1); close(done) }()
	go func() { io.Copy(b2, b2) }()
	a1.Write([]byte("hi"))
	got := make([]byte, 2)
	io.ReadFull(a1, got)
	if string(got) != "hi" {
		t.Fatalf("got %q", got)
	}
	a1.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not finish")
	}
}
