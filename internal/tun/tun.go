// Package tun is throne-connect's built-in system tunnel. It brings up a TUN
// device and forwards all traffic through the node's own SOCKS proxy (tun2socks),
// so Throne is no longer needed to carry traffic. (Throne still supplies the
// server list via throne.db; the tunnel is what moves here.)
//
// Only Linux is implemented; other platforms return ErrUnsupported.
package tun

import "errors"

// ErrUnsupported is returned by Start where the built-in TUN is not implemented.
var ErrUnsupported = errors.New("the built-in TUN is supported on Linux only")

// Options controls the tunnel.
type Options struct {
	Name      string   // TUN interface name
	Address   string   // interface address in CIDR form
	MTU       int      // interface MTU
	SocksAddr string   // host:port of the node's SOCKS proxy
	FWMark    int      // mark xray sets on its sockets; the bypass route matches it
	Table     int      // routing table id for the bypass policy route
	DNS       string   // if set, the system resolver while the tunnel is up
	Exclude   []string // extra destination CIDRs to keep off the tunnel
	LogLevel  string   // tun2socks log level: silent|error|warning|info|debug
}

// Tunnel is a running tunnel. Close tears it down (routes first, device last).
type Tunnel interface {
	Close() error
}
