// Package singbox generates the sing-box configuration for all VLESS
// tenants (one process, one tun inbound per tenant) and supervises nothing —
// procd owns the process, we only write the config.
package singbox

import (
	"encoding/json"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
)

type config struct {
	Log      *logCfg     `json:"log"`
	Inbounds []*inbound  `json:"inbounds"`
	Outbounds []*outbound `json:"outbounds"`
	Route    *route      `json:"route"`
}

type logCfg struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

type inbound struct {
	Type          string   `json:"type"`
	Tag           string   `json:"tag"`
	InterfaceName string   `json:"interface_name"`
	Address       []string `json:"address"`
	MTU           int      `json:"mtu"`
	AutoRoute     bool     `json:"auto_route"`
	StrictRoute   bool     `json:"strict_route"`
	Stack         string   `json:"stack"`
}

type outbound struct {
	Type       string  `json:"type"`
	Tag        string  `json:"tag"`
	Server     string  `json:"server,omitempty"`
	ServerPort int     `json:"server_port,omitempty"`
	UUID       string  `json:"uuid,omitempty"`
	Flow       string  `json:"flow,omitempty"`
	TLS        *tlsCfg `json:"tls,omitempty"`
}

type tlsCfg struct {
	Enabled    bool        `json:"enabled"`
	ServerName string      `json:"server_name,omitempty"`
	UTLS       *utlsCfg    `json:"utls,omitempty"`
	Reality    *realityCfg `json:"reality,omitempty"`
}

type utlsCfg struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"fingerprint"`
}

type realityCfg struct {
	Enabled  bool   `json:"enabled"`
	PublicKey string `json:"public_key"`
	ShortID  string `json:"short_id,omitempty"`
}

type route struct {
	Rules               []*rule `json:"rules"`
	Final               string  `json:"final"`
	AutoDetectInterface bool    `json:"auto_detect_interface"`
}

type rule struct {
	Inbound  []string `json:"inbound"`
	Outbound string   `json:"outbound"`
}

// Build renders the sing-box JSON. ok is false when no enabled tenant uses
// VLESS (sing-box should not run).
func Build(cfg *model.Config) (string, bool) {
	tenants := vlessTenants(cfg)
	if len(tenants) == 0 {
		return "", false
	}
	c := &config{
		Log: &logCfg{Level: "warn", Timestamp: true},
	}
	for _, t := range tenants {
		c.Inbounds = append(c.Inbounds, &inbound{
			Type:          "tun",
			Tag:           inboundTag(t),
			InterfaceName: t.TunName(),
			Address:       []string{t.TunAddr()},
			MTU:           1500,
			AutoRoute:     false,
			StrictRoute:   false,
			Stack:         "system",
		})
		v := t.VLESS
		ob := &outbound{
			Type:       "vless",
			Tag:        outboundTag(t),
			Server:     v.Server,
			ServerPort: v.Port,
			UUID:       v.UUID,
			Flow:       v.Flow,
		}
		switch v.Security {
		case "reality":
			ob.TLS = &tlsCfg{
				Enabled:    true,
				ServerName: v.SNI,
				UTLS:       &utlsCfg{Enabled: true, Fingerprint: v.Fingerprint},
				Reality: &realityCfg{
					Enabled:   true,
					PublicKey: v.PublicKey,
					ShortID:   v.ShortID,
				},
			}
		case "tls":
			ob.TLS = &tlsCfg{
				Enabled:    true,
				ServerName: v.SNI,
				UTLS:       &utlsCfg{Enabled: true, Fingerprint: v.Fingerprint},
			}
		}
		c.Outbounds = append(c.Outbounds, ob)
	}
	c.Outbounds = append(c.Outbounds, &outbound{Type: "direct", Tag: "direct"})
	c.Route = &route{Final: "direct", AutoDetectInterface: true}
	for _, t := range tenants {
		c.Route.Rules = append(c.Route.Rules, &rule{
			Inbound:  []string{inboundTag(t)},
			Outbound: outboundTag(t),
		})
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", false
	}
	return string(data), true
}

func vlessTenants(cfg *model.Config) []*model.Tenant {
	var out []*model.Tenant
	for _, t := range cfg.EnabledTenants() {
		if t.EgressMode == model.EgressVLESS {
			out = append(out, t)
		}
	}
	return out
}

func inboundTag(t *model.Tenant) string  { return "in-" + t.ID }
func outboundTag(t *model.Tenant) string { return "out-" + t.ID }
