package config

import (
	"log/slog"
	"strings"
	"testing"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

var minimal = map[string]string{
	"KARTE_EVENTS_URL":    "https://karte.example/integrations/receipt/events",
	"KARTE_INBOUND_TOKEN": "secret",
}

func with(extra map[string]string) map[string]string {
	merged := map[string]string{}
	for k, v := range minimal {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(minimal), "/opt/agent")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Adapter != AdapterOrcaPush || cfg.Orca.WSURL != "ws://localhost:8000/ws" || cfg.Orca.TenantID != "1" {
		t.Errorf("defaults: %+v", cfg)
	}
	// 退避先はバイナリの隣。カレントディレクトリに依らない。
	if cfg.SpoolDir != "/opt/agent/spool" {
		t.Errorf("SpoolDir = %q", cfg.SpoolDir)
	}
	if cfg.HealthAddr != "" || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("health %q level %v", cfg.HealthAddr, cfg.LogLevel)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		// 空のトークンで起動すると、通知のたびに 401 で弾かれ続けるだけになる。
		{"トークンなし", map[string]string{"KARTE_EVENTS_URL": minimal["KARTE_EVENTS_URL"]}, "KARTE_INBOUND_TOKEN"},
		{"送り先なし", map[string]string{"KARTE_INBOUND_TOKEN": "x"}, "KARTE_EVENTS_URL"},
		{"送り先が URL でない", with(map[string]string{"KARTE_EVENTS_URL": "karte.example"}), "KARTE_EVENTS_URL"},
		{"接続先が ws でない", with(map[string]string{"ORCA_WS_URL": "http://localhost:8000/ws"}), "ORCA_WS_URL"},
		{"証明書が片方だけ", with(map[string]string{"ORCA_TLS_CERT": "/c.crt"}), "ORCA_TLS_KEY"},
		{"知らないアダプタ", with(map[string]string{"AGENT_ADAPTER": "dicom-scp"}), "dicom-scp"},
		{"知らないログレベル", with(map[string]string{"LOG_LEVEL": "loud"}), "LOG_LEVEL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env), ".")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want mention of %s", err, tc.want)
			}
		})
	}
}

func TestParseDotenv(t *testing.T) {
	vals := parseDotenv(strings.NewReader(`
# comment
KARTE_EVENTS_URL=https://karte.example/events
export KARTE_INBOUND_TOKEN="quoted token"
ORCA_TENANT_ID = '1'
broken line
`))
	want := map[string]string{
		"KARTE_EVENTS_URL":    "https://karte.example/events",
		"KARTE_INBOUND_TOKEN": "quoted token",
		"ORCA_TENANT_ID":      "1",
	}
	for k, v := range want {
		if vals[k] != v {
			t.Errorf("%s = %q, want %q", k, vals[k], v)
		}
	}
	if len(vals) != len(want) {
		t.Errorf("parsed %d keys, want %d", len(vals), len(want))
	}
}
