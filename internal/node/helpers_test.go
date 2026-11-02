package node

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"throne-connect/internal/config"
	"throne-connect/internal/logbuf"
	"throne-connect/internal/source"
	"throne-connect/internal/xray"
)

const fakeXrayEnv = "THRONE_CONNECT_NODE_FAKE_XRAY"

// Outbounds containing rejectMarker are refused by the fake xray; a config with
// two or more conflictMarker outbounds is refused although each one alone passes.
const (
	rejectMarker   = "reject-me"
	conflictMarker = "conflict-me"
)

var fakeXrayBin string

func TestMain(m *testing.M) {
	if os.Getenv(fakeXrayEnv) == "1" {
		os.Exit(fakeXray(os.Args[1:]))
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeXrayBin = exe
	os.Setenv(fakeXrayEnv, "1")
	os.Exit(m.Run())
}

// fakeXray mimics `xray run -test -c f` (validation) and `xray run -c f` (serve).
// Serving is a no-op: tests own the per-server SOCKS ports themselves.
func fakeXray(args []string) int {
	if len(args) == 4 && args[0] == "run" && args[1] == "-test" && args[2] == "-c" {
		data, err := os.ReadFile(args[3])
		if err != nil {
			fmt.Println(err)
			return 2
		}
		s := string(data)
		if strings.Contains(s, rejectMarker) {
			fmt.Println("rejected outbound")
			return 1
		}
		if strings.Count(s, conflictMarker) >= 2 {
			fmt.Println("conflicting outbounds")
			return 1
		}
		return 0
	}
	if len(args) == 3 && args[0] == "run" && args[1] == "-c" {
		time.Sleep(time.Hour)
		return 0
	}
	fmt.Println("unexpected args", args)
	return 3
}

// socksServer is a minimal SOCKS5 CONNECT proxy standing in for one xray inbound.
type socksServer struct {
	ln    net.Listener
	fail  atomic.Bool  // close every new connection right away
	delay atomic.Int64 // nanoseconds to wait before answering CONNECT

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func newSocksServer(t *testing.T, ln net.Listener) *socksServer {
	s := &socksServer{ln: ln, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.close)
	return s
}

func startSocks(t *testing.T) *socksServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return newSocksServer(t, ln)
}

// startSocksRange starts n SOCKS servers on consecutive ports, as xray would
// listen on base_port+i, and returns the base port.
func startSocksRange(t *testing.T, n int) (int, []*socksServer) {
	t.Helper()
	for range 100 {
		base := 20000 + rand.IntN(40000)
		var lns []net.Listener
		for i := range n {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				break
			}
			lns = append(lns, ln)
		}
		if len(lns) == n {
			ss := make([]*socksServer, n)
			for i, ln := range lns {
				ss[i] = newSocksServer(t, ln)
			}
			return base, ss
		}
		for _, ln := range lns {
			ln.Close()
		}
	}
	t.Fatal("no free consecutive port range")
	return 0, nil
}

func (s *socksServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *socksServer) close() {
	s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.conns = nil
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *socksServer) add(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *socksServer) remove(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *socksServer) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		if s.fail.Load() {
			c.Close()
			continue
		}
		if !s.add(c) {
			c.Close()
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.remove(c)
			defer c.Close()
			s.handle(c)
		}()
	}
}

func (s *socksServer) handle(c net.Conn) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, hdr[1])); err != nil {
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 4:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(c, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return
		}
		host = string(name)
	default:
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return
	}
	if d := s.delay.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(pb))), 2*time.Second)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	if !s.add(up) {
		up.Close()
		return
	}
	defer s.remove(up)
	defer up.Close()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); up.Close(); done <- struct{}{} }()
	go func() { io.Copy(c, up); c.Close(); done <- struct{}{} }()
	<-done
	<-done
}

// socksDial opens a SOCKS5 CONNECT tunnel to target through proxy.
func socksDial(t *testing.T, proxy, target string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	host, port, _ := net.SplitHostPort(target)
	var p uint16
	fmt.Sscan(port, &p)
	req := []byte{5, 1, 0}
	req = append(req, 5, 1, 0, 1)
	req = append(req, net.ParseIP(host).To4()...)
	req = binary.BigEndian.AppendUint16(req, p)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 12)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatalf("socks handshake: %v", err)
	}
	if resp[3] != 0 {
		t.Fatalf("socks connect refused: %v", resp)
	}
	c.SetDeadline(time.Time{})
	return c
}

func startEcho(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Add(1)
			go func() { defer wg.Done(); io.Copy(c, c); c.Close() }()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

func startTarget(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type profile struct {
	name     string
	protocol string
	group    string
}

func outboundJSON(p profile) string {
	proto := p.protocol
	if proto == "" {
		proto = "vless"
	}
	return fmt.Sprintf(`{"protocol":%q,"tag":"t","settings":{"id":%q}}`, proto, p.name)
}

// writeThroneDB atomically replaces path with a Throne database holding profiles.
func writeThroneDB(t *testing.T, path string, profiles ...profile) {
	t.Helper()
	tmp := path + ".tmp"
	os.Remove(tmp)
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT, archive INTEGER)`,
		`CREATE TABLE groups_order (group_id INTEGER, display_order INTEGER)`,
		`CREATE TABLE profiles (id INTEGER PRIMARY KEY, name TEXT, gid INTEGER, outbound_json TEXT)`,
		`INSERT INTO groups VALUES (1, 'main', 0), (2, 'other', 0)`,
		`INSERT INTO groups_order VALUES (1, 1), (2, 2)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range profiles {
		gid := 1
		if p.group == "other" {
			gid = 2
		}
		if _, err := db.Exec(`INSERT INTO profiles VALUES (?, ?, ?, ?)`, i+1, p.name, gid, outboundJSON(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func testServer(name string) source.Server {
	return source.Server{Key: "key-" + name, Name: name, Group: "main", Outbound: []byte(outboundJSON(profile{name: name}))}
}

func newLogs() (*slog.Logger, *logbuf.Buffer) {
	buf := logbuf.New(1000)
	return slog.New(buf.Handler(slog.LevelDebug)), buf
}

func hasLog(buf *logbuf.Buffer, substr string) bool {
	lines, _ := buf.Since(0)
	for _, l := range lines {
		if strings.Contains(l.Text, substr) {
			return true
		}
	}
	return false
}

func countLog(buf *logbuf.Buffer, substr string) int {
	lines, _ := buf.Since(0)
	n := 0
	for _, l := range lines {
		if strings.Contains(l.Text, substr) {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func baseConfig(t *testing.T, testURL string) config.Config {
	return config.Config{
		Listen:         freeAddr(t),
		XrayLogLevel:   "warning",
		TestURL:        testURL,
		Timeout:        config.Duration{Duration: 2 * time.Second},
		CheckInterval:  config.Duration{Duration: time.Hour},
		ScanInterval:   config.Duration{Duration: time.Hour},
		ReloadInterval: config.Duration{Duration: time.Hour},
		FailThreshold:  2,
		DropOnFailover: true,
		DataDir:        t.TempDir(),
	}
}

// unitNode builds a node whose backends point at the given SOCKS servers,
// without starting xray or the run loop.
func unitNode(t *testing.T, cfg config.Config, servers ...*socksServer) (*Node, *logbuf.Buffer) {
	log, buf := newLogs()
	n := New(cfg, xray.NewRunner(fakeXrayBin, cfg.DataDir, log), log, buf)
	for i, s := range servers {
		n.backends = append(n.backends, &backend{
			srv:   testServer(fmt.Sprintf("srv%d", i)),
			port:  s.port(),
			conns: map[net.Conn]net.Conn{},
		})
	}
	if len(n.backends) > 0 {
		n.active.Store(n.backends[0])
	}
	return n, buf
}

func okResult(ms int) result { return result{ok: true, ms: ms, at: time.Now()} }
func failResult() result     { return result{err: "boom", at: time.Now()} }

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
