package singbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

func TestBuildNoVLESS(t *testing.T) {
	cfg, _ := uci.Parse([]byte(`
config tenant
	option id 'a'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'wan2'
	list networks 'lan'
`))
	m, _ := model.FromUCI(cfg)
	if _, ok := Build(m); ok {
		t.Fatal("no VLESS tenants must not produce config")
	}
}

func TestBuildVLESS(t *testing.T) {
	cfg, _ := uci.Parse([]byte(`
config tenant
	option id 'a'
	option name 'A'
	option enabled '1'
	option egress_mode 'vless'
	option vless_server 's.example'
	option vless_port '443'
	option vless_uuid 'uuid-1'
	option vless_flow 'xtls-rprx-vision'
	option vless_security 'reality'
	option vless_sni 's.example'
	option vless_fp 'chrome'
	option vless_pbk 'pbk'
	option vless_sid 'sid'
	list networks 'lan'

config tenant
	option id 'b'
	option name 'B'
	option enabled '1'
	option egress_mode 'vless'
	option vless_server 't.example'
	option vless_port '8443'
	option vless_uuid 'uuid-2'
	option vless_security 'tls'
	option vless_sni 't.example'
	list networks 'lan2'
`))
	m, _ := model.FromUCI(cfg)
	raw, ok := Build(m)
	if !ok {
		t.Fatal("expected config")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, raw)
	}
	inb := doc["inbounds"].([]any)
	if len(inb) != 2 {
		t.Fatalf("inbounds = %v", inb)
	}
	first := inb[0].(map[string]any)
	if first["type"] != "tun" || first["interface_name"] != "fb-tun1" || first["auto_route"] != false {
		t.Fatalf("tun inbound wrong: %v", first)
	}
	if !strings.Contains(raw, `"address"`) || !strings.Contains(raw, "10.231.1.1/30") {
		t.Fatalf("tun address missing")
	}
	ob := doc["outbounds"].([]any)
	if len(ob) != 3 { // a + b + direct
		t.Fatalf("outbounds = %v", ob)
	}
	route := doc["route"].(map[string]any)
	if route["auto_detect_interface"] != true || route["final"] != "direct" {
		t.Fatalf("route wrong: %v", route)
	}
	rules := route["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	if !strings.Contains(raw, `"reality"`) || !strings.Contains(raw, `"public_key": "pbk"`) {
		t.Fatalf("reality tls missing:\n%s", raw)
	}
	if !strings.Contains(raw, `"server_name": "t.example"`) {
		t.Fatalf("tls outbound missing")
	}
}
