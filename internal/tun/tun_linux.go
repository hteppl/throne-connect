//go:build linux

package tun

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

// bakResolv is where the original /etc/resolv.conf is parked while we override it.
const resolvConf = "/etc/resolv.conf"
const bakResolv = "/etc/resolv.conf.throne-connect.bak"

type linuxTunnel struct {
	opts     Options
	log      *slog.Logger
	gateway  string
	wanDev   string
	dnsSet   bool
	engineUp bool
}

// Start brings up the TUN device, points the default route at it and installs a
// policy route so xray's own (fwmark-tagged) traffic to the servers bypasses it.
func Start(_ context.Context, opts Options, log *slog.Logger) (Tunnel, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("the built-in TUN needs root (or CAP_NET_ADMIN); run as root")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return nil, fmt.Errorf("/dev/net/tun is missing (load the tun module): %w", err)
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return nil, fmt.Errorf("the `ip` command (iproute2) is required: %w", err)
	}
	gw, dev, err := defaultRoute()
	if err != nil {
		return nil, err
	}

	t := &linuxTunnel{opts: opts, log: log, gateway: gw, wanDev: dev}

	// Keep xray's traffic to the servers off the tunnel: a dedicated table routes
	// anything carrying the fwmark straight out of the real interface.
	table := strconv.Itoa(opts.Table)
	mark := strconv.Itoa(opts.FWMark)
	if err := ipRun(log, "route", "replace", "default", "via", gw, "dev", dev, "table", table); err != nil {
		return nil, err
	}
	// Add the rule only once.
	if !ruleExists(mark, table) {
		if err := ipRun(log, "rule", "add", "fwmark", mark, "table", table, "priority", "100"); err != nil {
			t.teardownRoutes()
			return nil, err
		}
	}

	// Start tun2socks: create the device and forward it to the node's SOCKS.
	engine.Insert(&engine.Key{
		Device:   "tun://" + opts.Name,
		Proxy:    "socks5://" + opts.SocksAddr,
		MTU:      opts.MTU,
		LogLevel: logLevel(opts.LogLevel),
	})
	engine.Start() // fatal on failure; preflight above covers the common causes
	t.engineUp = true

	// Address the interface and point the default route at it (split default so the
	// original default route stays in place for teardown and for the mark table).
	if err := t.setupDevice(); err != nil {
		t.Close()
		return nil, err
	}

	if opts.DNS != "" {
		if err := setResolv(opts.DNS); err != nil {
			log.Warn("tun: could not set DNS", "err", err)
		} else {
			t.dnsSet = true
		}
	}

	log.Info("tun up", "dev", opts.Name, "via", opts.SocksAddr, "bypass_fwmark", opts.FWMark)
	return t, nil
}

func (t *linuxTunnel) setupDevice() error {
	// Give the engine a moment to create the interface.
	for i := 0; i < 40; i++ {
		if _, err := os.Stat("/sys/class/net/" + t.opts.Name); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := ipRun(t.log, "addr", "replace", t.opts.Address, "dev", t.opts.Name); err != nil {
		return err
	}
	if err := ipRun(t.log, "link", "set", "dev", t.opts.Name, "mtu", strconv.Itoa(t.opts.MTU), "up"); err != nil {
		return err
	}
	// Keep the servers' own IPs and any extra excludes off the tunnel too.
	for _, cidr := range t.opts.Exclude {
		ipRun(t.log, "route", "replace", cidr, "via", t.gateway, "dev", t.wanDev)
	}
	if err := ipRun(t.log, "route", "replace", "0.0.0.0/1", "dev", t.opts.Name); err != nil {
		return err
	}
	return ipRun(t.log, "route", "replace", "128.0.0.0/1", "dev", t.opts.Name)
}

func (t *linuxTunnel) Close() error {
	if t.dnsSet {
		restoreResolv()
	}
	ipQuiet("route", "del", "0.0.0.0/1", "dev", t.opts.Name)
	ipQuiet("route", "del", "128.0.0.0/1", "dev", t.opts.Name)
	for _, cidr := range t.opts.Exclude {
		ipQuiet("route", "del", cidr, "via", t.gateway, "dev", t.wanDev)
	}
	t.teardownRoutes()
	if t.engineUp {
		engine.Stop()
		t.engineUp = false
	}
	t.log.Info("tun down", "dev", t.opts.Name)
	return nil
}

func (t *linuxTunnel) teardownRoutes() {
	ipQuiet("rule", "del", "fwmark", strconv.Itoa(t.opts.FWMark), "table", strconv.Itoa(t.opts.Table))
	ipQuiet("route", "flush", "table", strconv.Itoa(t.opts.Table))
}

// defaultRoute returns the gateway and interface of the current IPv4 default route.
func defaultRoute() (gw, dev string, err error) {
	out, err := exec.Command("ip", "-j", "route", "show", "default").Output()
	if err != nil {
		return "", "", fmt.Errorf("reading the default route: %w", err)
	}
	var routes []struct {
		Gateway string `json:"gateway"`
		Dev     string `json:"dev"`
	}
	if err := json.Unmarshal(out, &routes); err != nil {
		return "", "", fmt.Errorf("parsing `ip route`: %w", err)
	}
	for _, r := range routes {
		if r.Gateway != "" && r.Dev != "" {
			return r.Gateway, r.Dev, nil
		}
	}
	return "", "", fmt.Errorf("no IPv4 default route found (is the network up?)")
}

func ruleExists(mark, table string) bool {
	out, err := exec.Command("ip", "rule", "list").Output()
	if err != nil {
		return false
	}
	return len(out) > 0 && containsRule(string(out), mark, table)
}

func containsRule(rules, mark, table string) bool {
	// `ip rule` prints marks in hex; compare the decimal we set, both forms.
	m, _ := strconv.Atoi(mark)
	for _, needle := range []string{
		"fwmark " + mark + " lookup " + table,
		fmt.Sprintf("fwmark 0x%x lookup %s", m, table),
	} {
		if indexOf(rules, needle) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func ipRun(log *slog.Logger, args ...string) error {
	cmd := exec.Command("ip", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ip %v: %w: %s", args, err, out)
	}
	return nil
}

func ipQuiet(args ...string) { _ = exec.Command("ip", args...).Run() }

func setResolv(dns string) error {
	if _, err := os.Lstat(resolvConf); err == nil {
		if err := os.Rename(resolvConf, bakResolv); err != nil {
			return err
		}
	}
	return os.WriteFile(resolvConf, []byte("nameserver "+dns+"\n"), 0o644)
}

func restoreResolv() {
	os.Remove(resolvConf)
	os.Rename(bakResolv, resolvConf)
}

func logLevel(level string) string {
	switch level {
	case "silent", "error", "warning", "info", "debug":
		return level
	case "":
		return "warning"
	default:
		return "warning"
	}
}
