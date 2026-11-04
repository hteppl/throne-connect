// Package config loads throne-connect settings from a JSON file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Duration is a time.Duration that is written in JSON as a string ("10s", "5m").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

type Config struct {
	// Path to Throne's throne.db. Empty means the platform default.
	ThroneDB string `json:"throne_db"`
	// Only take profiles from these Throne groups (by name). Empty means all non-archived groups.
	Groups []string `json:"groups"`
	// Keep only profiles whose name contains one of these substrings (case-insensitive).
	Include []string `json:"include"`
	// Drop profiles whose name contains one of these substrings (case-insensitive).
	Exclude []string `json:"exclude"`

	// The single SOCKS5 endpoint Throne (or anything else) connects to.
	Listen string `json:"listen"`
	// Local HTTP API for status and manual switching. Empty disables it.
	APIListen string `json:"api_listen"`

	// Path to the xray binary. Empty means: next to throne-connect, then ./bin, then PATH.
	XrayPath     string `json:"xray_path"`
	XrayLogLevel string `json:"xray_log_level"`
	// Each server gets its own local SOCKS port starting here (127.0.0.1 only).
	BasePort int `json:"base_port"`
	// DNS servers xray uses to resolve VPN server hostnames. They must not depend on
	// the VPN itself, hence DoH in "+local" mode by default.
	DNS []string `json:"dns"`

	TestURL        string   `json:"test_url"`
	Timeout        Duration `json:"timeout"`
	CheckInterval  Duration `json:"check_interval"`
	ScanInterval   Duration `json:"scan_interval"`
	ReloadInterval Duration `json:"reload_interval"`
	// Consecutive failed checks of the active server before failing over.
	FailThreshold int `json:"fail_threshold"`
	// Move away from a healthy server only if another one is faster by this many ms.
	// 0 disables it: a working server is kept for as long as it works.
	SwitchBetterMs int `json:"switch_better_ms"`
	// Minimum time on a server before a "faster server" switch is allowed.
	MinDwell Duration `json:"min_dwell"`
	// On failover, close connections still open through the dead server so apps reconnect at once.
	DropOnFailover bool `json:"drop_on_failover"`

	// Where state and the generated xray config are kept. Empty means the platform default.
	DataDir string `json:"data_dir"`

	// Built-in system VPN. When enabled, throne-connect brings up a TUN device and
	// routes all traffic through the active server itself — no external client
	// (Throne) is needed for the tunnel. Linux only. Requires root / CAP_NET_ADMIN.
	TUN TUN `json:"tun"`
}

// TUN configures the built-in system tunnel (tun2socks over the node's own SOCKS).
type TUN struct {
	// Turn the tunnel on.
	Enabled bool `json:"enabled"`
	// TUN interface name.
	Name string `json:"name"`
	// Interface address (CIDR); a small unused range is enough.
	Address string `json:"address"`
	// Interface MTU.
	MTU int `json:"mtu"`
	// Firewall mark set on xray's own sockets so its traffic to the VPN servers
	// bypasses the tunnel (no loop). A policy route for this mark is installed.
	FWMark int `json:"fwmark"`
	// Routing table id used for the bypass policy route.
	Table int `json:"table"`
	// If set, point the system resolver here while the tunnel is up (so DNS goes
	// through the VPN); the previous /etc/resolv.conf is restored on shutdown.
	DNS string `json:"dns"`
	// Extra destination CIDRs to keep off the tunnel (LAN/private are kept direct anyway).
	Exclude []string `json:"exclude"`
}

func Default() Config {
	return Config{
		Listen:         "127.0.0.1:10808",
		APIListen:      "127.0.0.1:10809",
		XrayLogLevel:   "warning",
		BasePort:       21000,
		DNS:            []string{"https+local://1.1.1.1/dns-query", "https+local://8.8.8.8/dns-query", "https+local://77.88.8.8/dns-query"},
		TestURL:        "http://cp.cloudflare.com/generate_204",
		Timeout:        Duration{5 * time.Second},
		CheckInterval:  Duration{10 * time.Second},
		ScanInterval:   Duration{60 * time.Second},
		ReloadInterval: Duration{60 * time.Second},
		FailThreshold:  2,
		SwitchBetterMs: 0,
		MinDwell:       Duration{5 * time.Minute},
		DropOnFailover: true,
		TUN: TUN{
			Enabled: false,
			Name:    "throne0",
			Address: "198.18.0.1/30",
			MTU:     1500,
			FWMark:  1987,
			Table:   1987,
		},
	}
}

// DefaultPath is where `init` writes the config and where `run` looks for it.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "throne-connect.json"
	}
	return filepath.Join(dir, "throne-connect", "config.json")
}

// Load reads the config at path. With an empty path it tries ./throne-connect.json and
// DefaultPath(), falling back to defaults when neither exists. It returns the file used ("" if none).
func Load(path string) (Config, string, error) {
	cfg := Default()
	candidates := []string{path}
	if path == "" {
		candidates = []string{"throne-connect.json", DefaultPath()}
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) && path == "" {
			continue
		}
		if err != nil {
			return cfg, "", err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, "", fmt.Errorf("%s: %w", p, err)
		}
		return cfg, p, cfg.validate()
	}
	return cfg, "", cfg.validate()
}

func (c *Config) validate() error {
	if c.DataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("data_dir is not set and no user config dir: %w", err)
		}
		c.DataDir = filepath.Join(dir, "throne-connect")
	}
	switch {
	case c.Listen == "":
		return errors.New("listen must be set")
	case c.BasePort <= 0 || c.BasePort > 65000:
		return errors.New("base_port must be in 1..65000")
	case c.TestURL == "":
		return errors.New("test_url must be set")
	case c.FailThreshold < 1:
		return errors.New("fail_threshold must be >= 1")
	case c.Timeout.Duration <= 0 || c.CheckInterval.Duration <= 0 || c.ScanInterval.Duration <= 0 || c.ReloadInterval.Duration <= 0:
		return errors.New("timeout and intervals must be positive")
	}
	if c.TUN.Enabled {
		if c.TUN.Name == "" {
			c.TUN.Name = "throne0"
		}
		if c.TUN.Address == "" {
			c.TUN.Address = "198.18.0.1/30"
		}
		if c.TUN.MTU <= 0 {
			c.TUN.MTU = 1500
		}
		if c.TUN.FWMark <= 0 {
			c.TUN.FWMark = 1987
		}
		if c.TUN.Table <= 0 {
			c.TUN.Table = 1987
		}
	}
	return nil
}

// Marshal renders the config as indented JSON, for `init`.
func (c Config) Marshal() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}
