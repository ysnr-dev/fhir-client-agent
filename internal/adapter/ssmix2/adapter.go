// Package ssmix2 は SS-MIX2 標準化ストレージへの書き出しアダプタ。
//
// 向きが他のアダプタと逆で、カルテから HL7 v2.5 のメッセージを取りに行き、カルテが指定した
// ルートフォルダの下に標準化ストレージの規則(患者 ID / 診療日 / データ種別のフォルダと、
// コンディションフラグ付きのファイル名)で置く。電文の中身はカルテが組み、ここは触らない。
// 書けたかどうかは ack で返し、返せなかった分はカルテが配り直す(書き込みは冪等)。
package ssmix2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ysnr-dev/fhir-client-agent/internal/config"
	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// 1 回のポーリングで受け取る上限。カルテ側の上限(100)より小さくしておく。
const pollLimit = 50

type Adapter struct {
	cfg    config.Ssmix2Storage
	token  string
	client *http.Client
	log    *slog.Logger
	// trlog はトランザクションストレージ。設定が無ければ nil で、何もしない。
	trlog *TransactionLog
}

func New(cfg config.Ssmix2Storage, token string, log *slog.Logger) *Adapter {
	return &Adapter{cfg: cfg, token: token, client: &http.Client{Timeout: 60 * time.Second}, log: log,
		trlog: NewTransactionLog(cfg.TransactionDir, cfg.TransactionLineNo)}
}

func (a *Adapter) Name() string { return config.AdapterSsmix2Storage }

// response はカルテの GET …/messages の応答。
type response struct {
	Enabled  bool      `json:"enabled"`
	Storage  storage   `json:"storage"`
	Messages []Message `json:"messages"`
}

type storage struct {
	RootPath   string `json:"root_path"`
	FacilityID string `json:"facility_id"`
}

// Message はメッセージ 1 件。ヘッダー項目がフォルダとファイル名を決め、Body が電文(CP50221)。
type Message struct {
	ID             int64  `json:"id"`
	PatientID      string `json:"patient_id"`
	ServiceDate    string `json:"service_date"`
	DataKind       string `json:"data_kind"`
	OrderNo        string `json:"order_no"`
	Process        string `json:"process"`
	DepartmentCode string `json:"department_code"`
	TransactionAt  string `json:"transaction_at"`
	BodyBase64     string `json:"body_base64"`
}

type ackRequest struct {
	Written []int64      `json:"written"`
	Failed  []ackFailure `json:"failed"`
}

type ackFailure struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

// Run はカルテを定期的に取りに行き、受けたメッセージを順に書いて ack する。
// HTTP が通らなければエラーで返し、土台(agent.Agent)の再接続バックオフに任せる。
func (a *Adapter) Run(ctx context.Context, _ func(event.Event), ready func()) error {
	defer a.trlog.Close()
	connected := false
	for {
		res, err := a.poll(ctx)
		if err != nil {
			// 止められた途中のエラーは失敗ではない。
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !connected {
			connected = true
			ready()
		}

		if res.Enabled && len(res.Messages) > 0 {
			a.handle(ctx, res)
			// 溜まっている分は待たずに取りに行く。
			if len(res.Messages) >= pollLimit {
				continue
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.cfg.PollInterval):
		}
	}
}

func (a *Adapter) handle(ctx context.Context, res *response) {
	root := res.Storage.RootPath
	if err := checkRoot(root, a.cfg.AllowedRoot); err != nil {
		// 出力先の設定が間違っている。ack せずに置いておけば、直したあとに配り直される。
		a.log.Error("output root rejected; messages left on the karte", "root", root, "error", err)
		return
	}

	ack := ackRequest{Written: []int64{}, Failed: []ackFailure{}}
	for _, m := range res.Messages {
		outcome, err := Write(root, m)
		if err != nil {
			a.log.Error("write failed", "id", m.ID, "data_kind", m.DataKind, "error", err)
			ack.Failed = append(ack.Failed, ackFailure{ID: m.ID, Error: err.Error()})
			continue
		}
		a.log.Info("written", "id", m.ID, "data_kind", m.DataKind, "process", m.Process, "file", outcome.Path, "already", outcome.Existed)
		// トランザクションストレージは発生順の記録なので、配り直しで既にあった分は書かない。
		if !outcome.Existed {
			if err := a.trlog.Append(res.Storage.FacilityID, m, outcome.Body); err != nil {
				// 標準化ストレージには置けている。記録の写しが欠けたことだけ残す。
				a.log.Error("transaction log failed", "id", m.ID, "error", err)
			}
		}
		ack.Written = append(ack.Written, m.ID)
	}
	if err := a.ack(ctx, ack); err != nil {
		// 書けた分はファイルがあるので、配り直されても二重には書かない。
		a.log.Warn("ack failed; the karte will redeliver", "error", err)
	}
}

func (a *Adapter) poll(ctx context.Context) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?limit=%d", a.cfg.MessagesURL, pollLimit), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return nil, fmt.Errorf("poll: HTTP %d %s", res.StatusCode, snippet)
	}
	var out response
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("poll: %w", err)
	}
	return &out, nil
}

func (a *Adapter) ack(ctx context.Context, body ackRequest) error {
	if len(body.Written) == 0 && len(body.Failed) == 0 {
		return nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.MessagesURL+"/ack", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("ack: HTTP %d %s", res.StatusCode, snippet)
	}
	return nil
}

var errRootNotAllowed = errors.New("output root is outside SSMIX2_ALLOWED_ROOT")
