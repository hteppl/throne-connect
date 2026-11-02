package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{in: `"10s"`, want: 10 * time.Second},
		{in: `"1m30s"`, want: 90 * time.Second},
		{in: `10`, wantErr: "duration must be a string"},
		{in: `"ten seconds"`, wantErr: "invalid duration"},
		{in: `null`, wantErr: "invalid duration"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(tt.in), &d)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Duration != tt.want {
				t.Fatalf("got %v, want %v", d.Duration, tt.want)
			}
		})
	}
}

func TestDurationMarshal(t *testing.T) {
	b, err := json.Marshal(Duration{5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"5m0s"` {
		t.Fatalf("got %s", b)
	}
}

func TestDefaultIsValid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Default()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir == "" {
		t.Fatal("validate did not fill DataDir")
	}
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	got := DefaultPath()
	if !strings.HasPrefix(got, home) || !strings.HasSuffix(got, filepath.Join("throne-connect", "config.json")) {
		t.Fatalf("DefaultPath() = %q", got)
	}
}

func TestDefaultPathWithoutConfigDir(t *testing.T) {
	unsetConfigDir(t)
	if got := DefaultPath(); got != "throne-connect.json" {
		t.Fatalf("DefaultPath() = %q", got)
	}
}

// unsetConfigDir makes os.UserConfigDir fail on unix-like systems.
func unsetConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AppData", "")
	if _, err := os.UserConfigDir(); err == nil {
		t.Skip("cannot make os.UserConfigDir fail on this platform")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadExplicitPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	writeFile(t, p, `{
		"listen": "0.0.0.0:1080",
		"groups": ["A"],
		"base_port": 30000,
		"timeout": "2s",
		"min_dwell": "1m",
		"drop_on_failover": false,
		"data_dir": "`+filepath.ToSlash(dir)+`"
	}`)

	cfg, used, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if used != p {
		t.Fatalf("used = %q, want %q", used, p)
	}
	want := Default()
	want.Listen = "0.0.0.0:1080"
	want.Groups = []string{"A"}
	want.BasePort = 30000
	want.Timeout = Duration{2 * time.Second}
	want.MinDwell = Duration{time.Minute}
	want.DropOnFailover = false
	want.DataDir = filepath.ToSlash(dir)
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got  %+v\nwant %+v", cfg, want)
	}
}

func TestLoadExplicitPathErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	missing := filepath.Join(dir, "missing.json")
	if _, _, err := Load(missing); err == nil {
		t.Fatal("missing explicit file must be an error")
	}

	bad := filepath.Join(dir, "bad.json")
	writeFile(t, bad, `{"listen": `)
	_, used, err := Load(bad)
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("err = %v, want it to name the file", err)
	}
	if used != "" {
		t.Fatalf("used = %q on error", used)
	}

	badDur := filepath.Join(dir, "baddur.json")
	writeFile(t, badDur, `{"timeout": 5}`)
	if _, _, err := Load(badDur); err == nil {
		t.Fatal("numeric duration must be rejected")
	}

	invalid := filepath.Join(dir, "invalid.json")
	writeFile(t, invalid, `{"listen": ""}`)
	_, used, err = Load(invalid)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("err = %v", err)
	}
	if used != invalid {
		t.Fatalf("used = %q, want the file even when validation fails", used)
	}
}

func TestLoadSearchOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Chdir(t.TempDir())

	cfg, used, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if used != "" {
		t.Fatalf("used = %q with no config files", used)
	}
	if cfg.Listen != Default().Listen {
		t.Fatalf("defaults not applied: %+v", cfg)
	}

	writeFile(t, DefaultPath(), `{"listen": "127.0.0.1:2"}`)
	cfg, used, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if used != DefaultPath() || cfg.Listen != "127.0.0.1:2" {
		t.Fatalf("used = %q, listen = %q", used, cfg.Listen)
	}

	writeFile(t, "throne-connect.json", `{"listen": "127.0.0.1:1"}`)
	cfg, used, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if used != "throne-connect.json" || cfg.Listen != "127.0.0.1:1" {
		t.Fatalf("used = %q, listen = %q; the working directory must win", used, cfg.Listen)
	}
}

func TestLoadSearchUnreadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if err := os.Mkdir("throne-connect.json", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(""); err == nil {
		t.Fatal("an unreadable candidate must be an error, not skipped")
	}
}

func TestLoadWithoutConfigDir(t *testing.T) {
	unsetConfigDir(t)
	t.Chdir(t.TempDir())

	_, _, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "data_dir") {
		t.Fatalf("err = %v, want data_dir error", err)
	}

	writeFile(t, "throne-connect.json", `{"data_dir": "state"}`)
	cfg, used, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if used != "throne-connect.json" || cfg.DataDir != "state" {
		t.Fatalf("used = %q, data_dir = %q", used, cfg.DataDir)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(*Config)
		wantErr string
	}{
		{"ok", func(*Config) {}, ""},
		{"empty listen", func(c *Config) { c.Listen = "" }, "listen must be set"},
		{"zero base port", func(c *Config) { c.BasePort = 0 }, "base_port"},
		{"base port too high", func(c *Config) { c.BasePort = 65001 }, "base_port"},
		{"base port at limit", func(c *Config) { c.BasePort = 65000 }, ""},
		{"empty test url", func(c *Config) { c.TestURL = "" }, "test_url"},
		{"zero fail threshold", func(c *Config) { c.FailThreshold = 0 }, "fail_threshold"},
		{"zero timeout", func(c *Config) { c.Timeout.Duration = 0 }, "positive"},
		{"negative check interval", func(c *Config) { c.CheckInterval.Duration = -time.Second }, "positive"},
		{"zero scan interval", func(c *Config) { c.ScanInterval.Duration = 0 }, "positive"},
		{"zero reload interval", func(c *Config) { c.ReloadInterval.Duration = 0 }, "positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			c.DataDir = "/data"
			tt.mut(&c)
			err := c.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	in := Default()
	in.Groups = []string{"g"}
	in.DataDir = "/data"
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\n  \"timeout\": \"5s\"") {
		t.Fatalf("not indented or durations not strings:\n%s", b)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", in, out)
	}
}
