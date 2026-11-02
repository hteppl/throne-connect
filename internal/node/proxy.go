package node

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// serve accepts client connections and splices each one, byte for byte, onto the
// active server's xray SOCKS port. The SOCKS handshake itself passes through, so
// switching servers only changes where the next connection goes.
func (n *Node) serve(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			n.log.Warn("accept", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go n.handle(c)
	}
}

func (n *Node) handle(c net.Conn) {
	b := n.active.Load()
	if b == nil {
		c.Close()
		return
	}
	up, err := net.DialTimeout("tcp", b.addr(), 3*time.Second)
	if err != nil {
		n.log.Debug("xray unreachable", "err", err)
		c.Close()
		return
	}
	b.track(c, up)
	defer b.untrack(c)
	pipe(c, up)
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
