// Package xray builds the xray-core config and supervises the xray process.
package xray

import (
	"encoding/json"
	"fmt"

	"throne-connect/internal/source"
)

// InboundTag and OutboundTag name the per-server SOCKS inbound and its outbound.
func InboundTag(i int) string  { return fmt.Sprintf("in-%d", i) }
func OutboundTag(i int) string { return fmt.Sprintf("srv-%d", i) }

// BuildConfig produces an xray config where server i is reachable through a SOCKS
// inbound on 127.0.0.1:basePort+i that routes only to that server's outbound.
func BuildConfig(servers []source.Server, basePort int, dns []string, logLevel string) ([]byte, error) {
	inbounds := make([]any, 0, len(servers))
	outbounds := []any{map[string]any{"protocol": "freedom", "tag": "direct"}}
	rules := []any{map[string]any{"type": "field", "inboundTag": []string{"dns-internal"}, "outboundTag": "direct"}}

	for i, s := range servers {
		var ob map[string]any
		if err := json.Unmarshal(s.Outbound, &ob); err != nil {
			return nil, fmt.Errorf("server %q: %w", s.Name, err)
		}
		ob["tag"] = OutboundTag(i)
		// Resolve the server hostname with xray's own DNS rather than the system
		// resolver, whose queries Throne's TUN would send back into the VPN.
		ss, _ := ob["streamSettings"].(map[string]any)
		if ss == nil {
			ss = map[string]any{}
			ob["streamSettings"] = ss
		}
		so, _ := ss["sockopt"].(map[string]any)
		if so == nil {
			so = map[string]any{}
			ss["sockopt"] = so
		}
		if _, ok := so["domainStrategy"]; !ok {
			so["domainStrategy"] = "UseIPv4"
		}
		outbounds = append(outbounds, ob)

		inbounds = append(inbounds, map[string]any{
			"tag":      InboundTag(i),
			"listen":   "127.0.0.1",
			"port":     basePort + i,
			"protocol": "socks",
			"settings": map[string]any{"auth": "noauth", "udp": true, "ip": "127.0.0.1"},
		})
		rules = append(rules, map[string]any{
			"type": "field", "inboundTag": []string{InboundTag(i)}, "outboundTag": OutboundTag(i),
		})
	}

	cfg := map[string]any{
		"log": map[string]any{"loglevel": logLevel},
		"dns": map[string]any{
			"tag":           "dns-internal",
			"servers":       dns,
			"queryStrategy": "UseIPv4",
		},
		"policy": map[string]any{
			"levels": map[string]any{"0": map[string]any{
				"handshake": 8, "connIdle": 300, "uplinkOnly": 2, "downlinkOnly": 5,
			}},
		},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"routing":   map[string]any{"domainStrategy": "AsIs", "rules": rules},
	}
	return json.MarshalIndent(cfg, "", "  ")
}
