// Package config は環境変数から設定を読む。フラグは持たない。
// systemd の EnvironmentFile、コンテナの env、バイナリ隣の .env のどれでも
// 同じ名前で与えられるようにするため。
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Config はアダプタに依らない共通設定。
type Config struct {
	// Adapter はこのプロセスが動かす連携。1 プロセス 1 連携。
	Adapter string

	// EventsURL / InboundToken はカルテの受信エンドポイントと、その Bearer。
	EventsURL    string
	InboundToken string

	// SpoolDir は送れなかったイベントの退避先。
	SpoolDir string

	// HealthAddr が空なら待ち受けない(既定)。院内機に余計なポートを開けない。
	HealthAddr string

	LogLevel slog.Level

	Orca OrcaPush

	Ssmix2 Ssmix2Storage
}

// Ssmix2Storage は SS-MIX2 標準化ストレージへの書き出しアダプタの設定。
// カルテから HL7 メッセージを取りに行き、カルテが指定した出力先に書く。
type Ssmix2Storage struct {
	// MessagesURL はカルテの受け渡し口(…/integrations/ssmix2/messages)。
	MessagesURL string
	// PollInterval は取りに行く間隔。
	PollInterval time.Duration
	// AllowedRoot が空でなければ、カルテが指定した出力先はこの下でなければ書かない。
	AllowedRoot string
	// TransactionDir が空でなければ、置いたメッセージをトランザクションストレージとしてここにも追記する。
	TransactionDir string
	// TransactionLineNo はトランザクションデータファイル名の処理系統番号。
	TransactionLineNo string
}

// OrcaPush は日レセ PUSH 通知アダプタの設定。
type OrcaPush struct {
	WSURL    string
	TenantID string
	// クラウド版のみ。SSL クライアント認証。
	TLSCert string
	TLSKey  string
	TLSCA   string
}

const (
	AdapterOrcaPush      = "orca-push"
	AdapterSsmix2Storage = "ssmix2-storage"
)

const (
	defaultSsmix2PollInterval = 10 * time.Second
	defaultSsmix2LineNo       = "0001"
)

// Load は getenv から設定を組み立てて検証する。getenv を注入するのはテストのため。
func Load(getenv func(string) string, exeDir string) (Config, error) {
	cfg := Config{
		Adapter:      or(getenv("AGENT_ADAPTER"), AdapterOrcaPush),
		EventsURL:    strings.TrimSpace(getenv("KARTE_EVENTS_URL")),
		InboundToken: strings.TrimSpace(getenv("KARTE_INBOUND_TOKEN")),
		SpoolDir:     or(getenv("SPOOL_DIR"), filepath.Join(exeDir, "spool")),
		HealthAddr:   strings.TrimSpace(getenv("HEALTH_ADDR")),
		Orca: OrcaPush{
			// オンプレ(Ubuntu 22.04 以降)は API と同じ :8000。
			WSURL: or(getenv("ORCA_WS_URL"), "ws://localhost:8000/ws"),
			// オンプレはテナント ID が 1 固定。クラウドは証明書で決まり、この値は無視される。
			TenantID: or(getenv("ORCA_TENANT_ID"), "1"),
			TLSCert:  getenv("ORCA_TLS_CERT"),
			TLSKey:   getenv("ORCA_TLS_KEY"),
			TLSCA:    getenv("ORCA_TLS_CA"),
		},
		Ssmix2: Ssmix2Storage{
			MessagesURL:       strings.TrimSpace(getenv("KARTE_SSMIX2_URL")),
			AllowedRoot:       strings.TrimSpace(getenv("SSMIX2_ALLOWED_ROOT")),
			TransactionDir:    strings.TrimSpace(getenv("SSMIX2_TRANSACTION_DIR")),
			TransactionLineNo: or(getenv("SSMIX2_TRANSACTION_LINE_NO"), defaultSsmix2LineNo),
		},
	}

	level, err := parseLevel(getenv("LOG_LEVEL"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	var problems []string
	// 空のトークンで起動すると、通知のたびに 401 で弾かれ続けるだけになる。
	if cfg.InboundToken == "" {
		problems = append(problems, "KARTE_INBOUND_TOKEN が未設定です")
	}

	switch cfg.Adapter {
	case AdapterOrcaPush:
		// 通知を届ける先。ssmix2-storage は取りに行くだけなので要らない。
		if cfg.EventsURL == "" {
			problems = append(problems, "KARTE_EVENTS_URL が未設定です")
		} else if err := checkURL(cfg.EventsURL, "http", "https"); err != nil {
			problems = append(problems, "KARTE_EVENTS_URL: "+err.Error())
		}
		if err := checkURL(cfg.Orca.WSURL, "ws", "wss"); err != nil {
			problems = append(problems, "ORCA_WS_URL: "+err.Error())
		}
		if (cfg.Orca.TLSCert == "") != (cfg.Orca.TLSKey == "") {
			problems = append(problems, "ORCA_TLS_CERT と ORCA_TLS_KEY は両方指定してください")
		}
	case AdapterSsmix2Storage:
		if cfg.Ssmix2.MessagesURL == "" {
			problems = append(problems, "KARTE_SSMIX2_URL が未設定です")
		} else if err := checkURL(cfg.Ssmix2.MessagesURL, "http", "https"); err != nil {
			problems = append(problems, "KARTE_SSMIX2_URL: "+err.Error())
		}
		interval, err := parseInterval(getenv("SSMIX2_POLL_INTERVAL"))
		if err != nil {
			problems = append(problems, "SSMIX2_POLL_INTERVAL: "+err.Error())
		}
		cfg.Ssmix2.PollInterval = interval
		if cfg.Ssmix2.TransactionDir != "" && !filepath.IsAbs(cfg.Ssmix2.TransactionDir) && !strings.HasPrefix(cfg.Ssmix2.TransactionDir, `\\`) {
			problems = append(problems, "SSMIX2_TRANSACTION_DIR は絶対パスで指定してください")
		}
		if strings.ContainsAny(cfg.Ssmix2.TransactionLineNo, "_/\\.") {
			problems = append(problems, "SSMIX2_TRANSACTION_LINE_NO に _ やパス区切りは使えません")
		}
	default:
		problems = append(problems, fmt.Sprintf("AGENT_ADAPTER=%q は対応していません", cfg.Adapter))
	}

	if len(problems) > 0 {
		return Config{}, errors.New(strings.Join(problems, " / "))
	}
	return cfg, nil
}

func or(value, fallback string) string {
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return fallback
}

func checkURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	for _, s := range schemes {
		if u.Scheme == s && u.Host != "" {
			return nil
		}
	}
	return fmt.Errorf("%q は %s のいずれかで始まる URL にしてください", raw, strings.Join(schemes, " / "))
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "INFO":
		return slog.LevelInfo, nil
	case "DEBUG":
		return slog.LevelDebug, nil
	case "WARN", "WARNING":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("LOG_LEVEL=%q は DEBUG / INFO / WARN / ERROR のいずれかにしてください", raw)
}

// parseInterval は "10s" / "1m" の形。空なら既定。1 秒未満は連打になるので弾く。
func parseInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultSsmix2PollInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q は 10s や 1m の形で指定してください", raw)
	}
	if d < time.Second {
		return 0, fmt.Errorf("%q は 1 秒以上にしてください", raw)
	}
	return d, nil
}
