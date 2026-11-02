package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"throne-connect/internal/config"
)

const (
	envFakeXray = "THRONE_CONNECT_TEST_FAKE_XRAY"
	envMainArgs = "THRONE_CONNECT_TEST_MAIN_ARGS"
)

func TestMain(m *testing.M) {
	if os.Getenv(envFakeXray) == "1" {
		os.Exit(fakeXray(os.Args[1:]))
	}
	if args, ok := os.LookupEnv(envMainArgs); ok {
		var list []string
		if err := json.Unmarshal([]byte(args), &list); err != nil {
			panic(err)
		}
		os.Args = append([]string{"throne-connect"}, list...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeXray mimics `xray run -test -c f` (accept any config) and `xray run -c f`
// (listen on every inbound port until killed).
func fakeXray(args []string) int {
	var cfgPath string
	test := false
	for i, a := range args {
		switch a {
		case "-test":
			test = true
		case "-c":
			if i+1 < len(args) {
				cfgPath = args[i+1]
			}
		}
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return 1
	}
	if test {
		return 0
	}
	var cfg struct {
		Inbounds []struct {
			Listen string `json:"listen"`
			Port   int    `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return 1
	}
	for _, in := range cfg.Inbounds {
		ln, err := net.Listen("tcp", net.JoinHostPort(in.Listen, strconv.Itoa(in.Port)))
		if err != nil {
			return 1
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
	select {}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// freeBasePort returns a free port that config validation accepts as base_port.
func freeBasePort(t *testing.T) int {
	t.Helper()
	for {
		if p := freePort(t); p <= 65000 {
			return p
		}
	}
}

func freeAddr(t *testing.T) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
}

// throneDB builds a minimal throne.db with two xray profiles, one sing-box
// profile and one profile in an archived group.
func throneDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "throne.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT, archive INTEGER)`,
		`CREATE TABLE groups_order (group_id INTEGER, display_order INTEGER)`,
		`CREATE TABLE profiles (id INTEGER PRIMARY KEY, name TEXT, gid INTEGER, outbound_json TEXT)`,
		`INSERT INTO groups VALUES (1, 'Main', 0), (2, 'Old', 1)`,
		`INSERT INTO groups_order VALUES (1, 0), (2, 1)`,
		`INSERT INTO profiles VALUES
			(1, 'Amsterdam', 1, '{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443}]}}'),
			(2, 'Berlin', 1, '{"protocol":"trojan","settings":{"servers":[{"address":"b.example","port":443}]}}'),
			(3, 'SingBox', 1, '{"type":"vless","server":"c.example"}'),
			(4, 'Archived', 2, '{"protocol":"vless","settings":{"vnext":[{"address":"d.example","port":443}]}}')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// nodeConfig returns a config for a node with the fake xray, the fixture db and
// free ports, plus the path of the same config written to disk.
func nodeConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envFakeXray, "1")
	cfg := config.Default()
	cfg.ThroneDB = throneDB(t)
	cfg.XrayPath = exe
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Listen = freeAddr(t)
	cfg.APIListen = freeAddr(t)
	cfg.BasePort = freeBasePort(t)
	cfg.TestURL = "http://127.0.0.1:1/"
	cfg.Timeout = config.Duration{Duration: 200 * time.Millisecond}
	cfg.DNS = nil
	return cfg, writeConfig(t, cfg)
}

func writeConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureStdout redirects os.Stdout while fn runs and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { io.Copy(&buf, r); close(done) }()
	func() {
		defer func() { os.Stdout = orig; w.Close() }()
		fn()
	}()
	<-done
	r.Close()
	return buf.String()
}

// waitAPI blocks until the node API at addr answers /status.
func waitAPI(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("node API at %s did not come up", addr)
}
