// Package orcapush は日レセ(WebORCA)の PUSH 通知を受けるアダプタ。
//
// 日レセの語彙 — イベント名、電文の項目名、テナントのヘッダ — はこのパッケージで
// 完結させる。外へ出すのは中立イベントだけ。
package orcapush

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/ysnr-dev/fhir-client-agent/internal/config"
	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// 購読の応答を待つ時間。繋がったのに購読できないまま黙って待ち続けないように。
const subscribeTimeout = 15 * time.Second

// 無通信でも接続が生きているか確かめる間隔。院内の NAT やプロキシが黙って
// 切った接続は、読み取りだけでは気づけない。
const pingEvery = 30 * time.Second

type Adapter struct {
	cfg        config.OrcaPush
	httpClient *http.Client
	log        *slog.Logger
}

func New(cfg config.OrcaPush, log *slog.Logger) (*Adapter, error) {
	client, err := httpClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{cfg: cfg, httpClient: client, log: log}, nil
}

func (a *Adapter) Name() string { return config.AdapterOrcaPush }

// frame は日レセとやり取りする 1 メッセージ。キーにドットが入るのは仕様。
type frame struct {
	Command string          `json:"command"`
	ReqID   string          `json:"req.id,omitempty"`
	SubID   string          `json:"sub.id,omitempty"`
	Event   string          `json:"event,omitempty"`
	Code    string          `json:"code,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (a *Adapter) Run(ctx context.Context, emit func(event.Event), ready func()) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, a.cfg.WSURL, &websocket.DialOptions{
		HTTPClient: a.httpClient,
		// オンプレは認証が無く、テナントはクライアントが名乗る。
		// クラウドは証明書でテナントが決まり、このヘッダは無視される。
		HTTPHeader: http.Header{"X-GINBEE-TENANT-ID": []string{a.cfg.TenantID}},
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", a.cfg.WSURL, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	// 種別を絞らず全部購読する。要るものの取捨は translate が決める。
	subscribe, _ := json.Marshal(frame{Command: "subscribe", ReqID: newUUID(), Event: "*"})
	if err := conn.Write(ctx, websocket.MessageText, subscribe); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	subscribed := false
	deadline := time.AfterFunc(subscribeTimeout, func() {
		if !subscribed {
			conn.Close(websocket.StatusPolicyViolation, "subscribe timed out")
		}
	})
	defer deadline.Stop()

	go a.keepAlive(ctx, conn)

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				conn.Close(websocket.StatusNormalClosure, "shutting down")
				return nil
			}
			if !subscribed {
				return fmt.Errorf("closed before subscribed: %w", err)
			}
			return err
		}

		var f frame
		if err := json.Unmarshal(raw, &f); err != nil {
			a.log.Error("unparsable message", "error", err)
			continue
		}

		switch f.Command {
		case "subscribed":
			subscribed = true
			a.log.Info("subscribed", "sub_id", f.SubID)
			ready()
		case "error":
			a.log.Error("orca error", "code", f.Code, "reason", f.Reason)
			if !subscribed {
				return errors.New("subscribe rejected: " + f.Code + " " + f.Reason)
			}
		case "event":
			var n notification
			if err := json.Unmarshal(f.Data, &n); err != nil {
				a.log.Error("unparsable event", "error", err)
				continue
			}
			e, ok := translate(n)
			if !ok {
				a.log.Debug("ignored", "event", n.Event)
				continue
			}
			a.log.Info("forwarding", "type", e.Type, "patient_number", e.PatientNumber, "event_id", e.ID)
			emit(e)
		}
	}
}

func (a *Adapter) keepAlive(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				// 読み取り側を起こして再接続へ進ませる。
				conn.Close(websocket.StatusGoingAway, "ping failed")
				return
			}
		}
	}
}

func httpClient(cfg config.OrcaPush) (*http.Client, error) {
	if cfg.TLSCert == "" && cfg.TLSCA == "" {
		return http.DefaultClient, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSCert != "" {
		pair, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("クライアント証明書を読めません: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{pair}
	}
	if cfg.TLSCA != "" {
		pem, err := os.ReadFile(cfg.TLSCA)
		if err != nil {
			return nil, fmt.Errorf("CA 証明書を読めません: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA 証明書を解釈できません")
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}
