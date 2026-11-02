package source

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	vlessA = `{"protocol":"vless","tag":"A-tag","settings":{"vnext":[{"address":"a.example","port":443}]}}`
	vlessB = `{"protocol":"vless","settings":{"vnext":[{"address":"b.example","port":443}]}}`
	trojan = `{"protocol":"trojan","tag":"from-tag","settings":{"servers":[{"address":"c.example","port":443}]}}`
	ssNone = `{"protocol":"shadowsocks","settings":{"servers":[{"address":"d.example","port":8388}]}}`
)

type profile struct {
	id       int
	name     any
	gid      int
	outbound any
}

type group struct {
	id      int
	name    string
	archive int
	order   int
}

func createDB(t *testing.T, path string, withOrder bool, groups []group, profiles []profile) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(path)}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE groups (id INTEGER PRIMARY KEY, name TEXT, archive INTEGER)`,
		`CREATE TABLE profiles (id INTEGER PRIMARY KEY, name TEXT, gid INTEGER, outbound_json TEXT)`,
	}
	if withOrder {
		stmts = append(stmts, `CREATE TABLE groups_order (group_id INTEGER, display_order INTEGER)`)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range groups {
		if _, err := db.Exec(`INSERT INTO groups VALUES (?, ?, ?)`, g.id, g.name, g.archive); err != nil {
			t.Fatal(err)
		}
		if withOrder {
			if _, err := db.Exec(`INSERT INTO groups_order VALUES (?, ?)`, g.id, g.order); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, p := range profiles {
		if _, err := db.Exec(`INSERT INTO profiles VALUES (?, ?, ?, ?)`, p.id, p.name, p.gid, p.outbound); err != nil {
			t.Fatal(err)
		}
	}
}

func fixture(t *testing.T, withOrder bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "throne.db")
	createDB(t, path, withOrder,
		[]group{
			{id: 1, name: "Main", order: 2},
			{id: 2, name: "Backup", order: 1},
			{id: 3, name: "Old", archive: 1, order: 0},
		},
		[]profile{
			{id: 10, name: "NL Amsterdam", gid: 1, outbound: vlessA},
			{id: 11, name: "", gid: 1, outbound: trojan},
			{id: 12, name: nil, gid: 1, outbound: ssNone},
			{id: 13, name: "sing-box", gid: 1, outbound: `{"type":"vless","server":"x"}`},
			{id: 14, name: "broken", gid: 1, outbound: `not json`},
			{id: 15, name: "NL duplicate", gid: 1, outbound: strings.Replace(vlessA, `"A-tag"`, `"other-tag"`, 1)},
			{id: 20, name: "DE Frankfurt", gid: 2, outbound: vlessB},
			{id: 30, name: "archived", gid: 3, outbound: `{"protocol":"vmess"}`},
			{id: 40, name: "orphan", gid: 99, outbound: `{"protocol":"socks"}`},
		})
	return path
}

func names(servers []Server) []string {
	var out []string
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}

func TestLoadThrone(t *testing.T) {
	tests := []struct {
		name      string
		withOrder bool
		want      []string
	}{
		{"groups_order decides group order", true, []string{"DE Frankfurt", "NL Amsterdam", "from-tag", "profile-12", "orphan"}},
		{"legacy schema orders by gid", false, []string{"NL Amsterdam", "from-tag", "profile-12", "DE Frankfurt", "orphan"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers, err := LoadThrone(fixture(t, tt.withOrder), nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := names(servers); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadThroneServerFields(t *testing.T) {
	servers, err := LoadThrone(fixture(t, false), []string{"Main"}, []string{"amsterdam"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers", len(servers))
	}
	s := servers[0]
	if s.Name != "NL Amsterdam" || s.Group != "Main" || s.ProfileID != 10 {
		t.Fatalf("unexpected server %+v", s)
	}
	if len(s.Key) != 12 {
		t.Fatalf("key %q is not 6 hex bytes", s.Key)
	}
	var ob map[string]any
	if err := json.Unmarshal(s.Outbound, &ob); err != nil {
		t.Fatal(err)
	}
	if _, ok := ob["tag"]; ok {
		t.Fatal("tag must be stripped from the outbound")
	}
	if ob["protocol"] != "vless" {
		t.Fatalf("outbound = %s", s.Outbound)
	}
}

func TestLoadThroneKeyIgnoresTagAndKeyOrder(t *testing.T) {
	dir := t.TempDir()
	p1, p2 := filepath.Join(dir, "1.db"), filepath.Join(dir, "2.db")
	createDB(t, p1, false, nil, []profile{{id: 1, name: "x", outbound: `{"protocol":"vless","tag":"a","settings":{"k":1}}`}})
	createDB(t, p2, false, nil, []profile{{id: 7, name: "y", outbound: `{"settings":{"k":1},"tag":"b","protocol":"vless"}`}})
	s1, err := LoadThrone(p1, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LoadThrone(p2, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s1[0].Key != s2[0].Key {
		t.Fatalf("keys differ: %s vs %s", s1[0].Key, s2[0].Key)
	}
}

func TestLoadThroneFilters(t *testing.T) {
	tests := []struct {
		name                     string
		groups, include, exclude []string
		want                     []string
	}{
		{"group", []string{"Backup"}, nil, nil, []string{"DE Frankfurt"}},
		{"archived group stays hidden", []string{"Old"}, nil, nil, nil},
		{"include is case-insensitive", nil, []string{"FRANKFURT", "profile"}, nil, []string{"profile-12", "DE Frankfurt"}},
		{"exclude", nil, nil, []string{"nl ", "Orphan"}, []string{"from-tag", "profile-12", "DE Frankfurt"}},
		{"include then exclude", []string{"Main"}, []string{"a"}, []string{"tag"}, []string{"NL Amsterdam"}},
	}
	path := fixture(t, false)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers, err := LoadThrone(path, tt.groups, tt.include, tt.exclude)
			if err != nil {
				t.Fatal(err)
			}
			if got := names(servers); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadThroneDuplicateAfterFilter(t *testing.T) {
	servers, err := LoadThrone(fixture(t, false), nil, []string{"duplicate"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(servers); !reflect.DeepEqual(got, []string{"NL duplicate"}) {
		t.Fatalf("got %q", got)
	}
}

func TestLoadThroneErrors(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadThrone(filepath.Join(dir, "missing.db"), nil, nil, nil); err == nil || !strings.Contains(err.Error(), "throne db") {
		t.Fatalf("err = %v", err)
	}

	empty := filepath.Join(dir, "empty.db")
	db, err := sql.Open("sqlite", empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE other (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := LoadThrone(empty, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "throne db query") {
		t.Fatalf("err = %v", err)
	}

	nullOutbound := filepath.Join(dir, "null.db")
	createDB(t, nullOutbound, false, nil, []profile{{id: 1, name: "x", outbound: nil}})
	if _, err := LoadThrone(nullOutbound, nil, nil, nil); err == nil {
		t.Fatal("NULL outbound_json must surface as a scan error")
	}
}

func TestLoadThroneDoesNotWrite(t *testing.T) {
	path := fixture(t, true)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadThrone(path, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("LoadThrone modified the database")
	}
	if _, err := os.Stat(path + "-journal"); err == nil {
		t.Fatal("LoadThrone left a journal")
	}
}

func TestLoadThroneUnusualPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my dir #1 %20 ?x")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "throne.db")
	createDB(t, path, false, nil, []profile{{id: 1, name: "only", outbound: vlessB}})
	servers, err := LoadThrone(path, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(servers); !reflect.DeepEqual(got, []string{"only"}) {
		t.Fatalf("got %q", got)
	}
}

func TestLoadThroneDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	path := DefaultThroneDB()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	createDB(t, path, true, nil, []profile{{id: 1, name: "default", outbound: vlessB}})
	servers, err := LoadThrone("", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(servers); !reflect.DeepEqual(got, []string{"default"}) {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultThroneDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	got := DefaultThroneDB()
	if !strings.HasPrefix(got, home) || !strings.HasSuffix(got, filepath.Join("Throne", "config", "throne.db")) {
		t.Fatalf("DefaultThroneDB() = %q", got)
	}

	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := DefaultThroneDB(); got != "" {
		t.Fatalf("without a config dir got %q, want empty", got)
	}
}

func TestDSN(t *testing.T) {
	got := dsn("/tmp/a b/throne.db")
	want := "file:/tmp/a%20b/throne.db?mode=ro&_pragma=busy_timeout(5000)"
	if runtime.GOOS == "windows" {
		return
	}
	if got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
}

func TestFingerprint(t *testing.T) {
	a := Server{Key: "aa", Name: "A"}
	b := Server{Key: "bb", Name: "B"}
	fp := Fingerprint([]Server{a, b})
	if len(fp) != 16 {
		t.Fatalf("fingerprint %q is not 8 hex bytes", fp)
	}
	if fp != Fingerprint([]Server{a, b}) {
		t.Fatal("fingerprint is not deterministic")
	}
	others := map[string][]Server{
		"order":  {b, a},
		"rename": {a, {Key: "bb", Name: "B2"}},
		"key":    {a, {Key: "bc", Name: "B"}},
		"subset": {a},
	}
	for name, s := range others {
		if Fingerprint(s) == fp {
			t.Errorf("%s: fingerprint did not change", name)
		}
	}
	if Fingerprint(nil) != Fingerprint([]Server{}) {
		t.Error("nil and empty must match")
	}
}
