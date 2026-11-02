// Package source reads VPN servers from Throne's SQLite database.
package source

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	_ "modernc.org/sqlite"
)

// Server is one VPN endpoint taken from a Throne profile.
type Server struct {
	// Key identifies the server by its connection parameters, so it survives
	// subscription refreshes that renumber or rename profiles.
	Key       string          `json:"key"`
	Name      string          `json:"name"`
	Group     string          `json:"group"`
	ProfileID int             `json:"profile_id"`
	Outbound  json.RawMessage `json:"outbound"` // xray outbound object
}

// DefaultThroneDB returns where Throne keeps its database on this platform.
func DefaultThroneDB() string {
	dir, err := os.UserConfigDir() // ~/.config on Linux, %APPDATA% on Windows
	if err != nil {
		return ""
	}
	p := filepath.Join(dir, "Throne", "config", "throne.db")
	if runtime.GOOS == "windows" {
		// Portable Throne keeps config next to Throne.exe; the common install paths are checked too.
		for _, c := range []string{
			p,
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Throne", "config", "throne.db"),
			filepath.Join(os.Getenv("ProgramFiles"), "Throne", "config", "throne.db"),
		} {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	return p
}

// LoadThrone reads xray-format profiles from throne.db (read-only), filtered by
// group names and name substrings. Profiles that are not xray outbounds are skipped.
func LoadThrone(path string, groups, include, exclude []string) ([]Server, error) {
	if path == "" {
		path = DefaultThroneDB()
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("throne db: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT p.id, COALESCE(p.name, ''), COALESCE(g.name, ''), p.outbound_json
		FROM profiles p
		LEFT JOIN groups g ON g.id = p.gid
		LEFT JOIN groups_order o ON o.group_id = p.gid
		WHERE COALESCE(g.archive, 0) = 0
		ORDER BY COALESCE(o.display_order, p.gid), p.id`)
	if err != nil {
		// Older schemas may lack groups_order; retry without it.
		rows, err = db.Query(`
			SELECT p.id, COALESCE(p.name, ''), COALESCE(g.name, ''), p.outbound_json
			FROM profiles p
			LEFT JOIN groups g ON g.id = p.gid
			WHERE COALESCE(g.archive, 0) = 0
			ORDER BY p.gid, p.id`)
		if err != nil {
			return nil, fmt.Errorf("throne db query: %w", err)
		}
	}
	defer rows.Close()

	var out []Server
	seen := map[string]bool{}
	for rows.Next() {
		var s Server
		var raw string
		if err := rows.Scan(&s.ProfileID, &s.Name, &s.Group, &raw); err != nil {
			return nil, err
		}
		var ob map[string]any
		if json.Unmarshal([]byte(raw), &ob) != nil {
			continue
		}
		if _, ok := ob["protocol"].(string); !ok {
			continue // sing-box format profile; only xray outbounds are supported
		}
		if s.Name == "" {
			s.Name, _ = ob["tag"].(string)
		}
		if s.Name == "" {
			s.Name = fmt.Sprintf("profile-%d", s.ProfileID)
		}
		if !keep(s, groups, include, exclude) {
			continue
		}
		delete(ob, "tag")
		canon, _ := json.Marshal(ob) // map keys are sorted, so this is canonical
		sum := sha256.Sum256(canon)
		s.Key = hex.EncodeToString(sum[:6])
		if seen[s.Key] {
			continue
		}
		seen[s.Key] = true
		s.Outbound = canon
		out = append(out, s)
	}
	return out, rows.Err()
}

func keep(s Server, groups, include, exclude []string) bool {
	if len(groups) > 0 && !slices.Contains(groups, s.Group) {
		return false
	}
	name := strings.ToLower(s.Name)
	has := func(list []string) bool {
		return slices.ContainsFunc(list, func(sub string) bool { return strings.Contains(name, strings.ToLower(sub)) })
	}
	if len(include) > 0 && !has(include) {
		return false
	}
	return !has(exclude)
}

func dsn(path string) string {
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Path: p}
	return "file:" + u.EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)"
}

// Fingerprint changes whenever the set of servers or their order changes.
func Fingerprint(servers []Server) string {
	h := sha256.New()
	for _, s := range servers {
		h.Write([]byte(s.Key))
		h.Write([]byte(s.Name))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
