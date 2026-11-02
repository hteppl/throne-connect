package node

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"throne-connect/internal/source"
)

const historySize = 10

type result struct {
	ok  bool
	ms  int
	err string
	at  time.Time
}

// backend is one VPN server reachable through its own local xray SOCKS port.
// Health fields are guarded by Node.mu; conns by connMu.
type backend struct {
	srv  source.Server
	port int

	hist     []result
	fails    int // consecutive failed checks
	lastOK   time.Time
	okTotal  int
	errTotal int

	connMu sync.Mutex
	conns  map[net.Conn]net.Conn // client -> xray, currently open
	dials  int64                 // total connections opened to this server
}

func (b *backend) addr() string { return fmt.Sprintf("127.0.0.1:%d", b.port) }

func (b *backend) record(r result) {
	b.hist = append(b.hist, r)
	if len(b.hist) > historySize {
		b.hist = b.hist[len(b.hist)-historySize:]
	}
	if r.ok {
		b.fails = 0
		b.lastOK = r.at
		b.okTotal++
	} else {
		b.fails++
		b.errTotal++
	}
}

// healthy means the last check passed.
func (b *backend) healthy() bool {
	return len(b.hist) > 0 && b.hist[len(b.hist)-1].ok
}

// score is the median recent latency plus a penalty per recent failure, so a
// fast but flaky server ranks below a slightly slower stable one. Lower is better.
func (b *backend) score() float64 {
	var lats []int
	fails := 0
	for _, r := range b.hist {
		if r.ok {
			lats = append(lats, r.ms)
		} else {
			fails++
		}
	}
	if len(lats) == 0 {
		return math.Inf(1)
	}
	slices.Sort(lats)
	return float64(lats[len(lats)/2]) + float64(fails)*1000
}

func (b *backend) lastMs() int {
	for i := len(b.hist) - 1; i >= 0; i-- {
		if b.hist[i].ok {
			return b.hist[i].ms
		}
	}
	return 0
}

func (b *backend) track(c, up net.Conn) {
	b.connMu.Lock()
	b.conns[c] = up
	b.dials++
	b.connMu.Unlock()
}

func (b *backend) untrack(c net.Conn) {
	b.connMu.Lock()
	delete(b.conns, c)
	b.connMu.Unlock()
}

func (b *backend) connCount() int {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	return len(b.conns)
}

// dialCount is the total number of connections ever opened to this server.
func (b *backend) dialCount() int64 {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	return b.dials
}

// closeAll drops every client connection going through this server.
func (b *backend) closeAll() int {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	n := len(b.conns)
	for c, up := range b.conns {
		c.Close()
		up.Close()
	}
	return n
}

// probe fetches testURL through the server and measures how long it took.
func probe(ctx context.Context, port int, testURL string, timeout time.Duration) result {
	start := time.Now()
	tr := &http.Transport{
		Proxy:             http.ProxyURL(&url.URL{Scheme: "socks5", Host: fmt.Sprintf("127.0.0.1:%d", port)}),
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return result{err: err.Error(), at: start}
	}
	resp, err := client.Do(req)
	if err != nil {
		return result{err: shortErr(err), at: start}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return result{err: "HTTP " + resp.Status, at: start}
	}
	return result{ok: true, ms: int(time.Since(start).Milliseconds()), at: start}
}

func shortErr(err error) string {
	var ue *url.Error
	for {
		if e, ok := err.(*url.Error); ok {
			ue = e
			err = ue.Err
			continue
		}
		break
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
