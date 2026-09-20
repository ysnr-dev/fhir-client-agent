// Package deliver はイベントをカルテへ届ける。
//
// 連携先の通知は未接続中のぶんが再配達されないことが多い(日レセは受信確認も
// 未配達保存も持たない)。受け取ったものはここで落とさない: 送れなければ
// ディスクに退避し、あとで順番どおりに送り直す。
package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// Deliverer は 1 本の送信キューを持ち、受け取った順に届ける。
type Deliverer struct {
	url      string
	token    string
	spoolDir string
	client   *http.Client
	log      *slog.Logger

	// Backoff は 1 イベントあたりの再送間隔。尽きたら退避する。
	Backoff []time.Duration
	// DrainEvery は退避ぶんを送り直す間隔。
	DrainEvery time.Duration

	queue chan event.Event
	mu    sync.Mutex
	seq   int
}

// New は Deliverer を作る。spoolDir はここで作成する。
func New(url, token, spoolDir string, log *slog.Logger) (*Deliverer, error) {
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("spool を作れません: %w", err)
	}
	return &Deliverer{
		url:      url,
		token:    token,
		spoolDir: spoolDir,
		// 常駐プロセスなのでタイムアウトは必須。カルテ側は同期で連携先へ取り直しに
		// 行くので、読み取りは長めに取る。
		client:     &http.Client{Timeout: 60 * time.Second},
		log:        log,
		Backoff:    []time.Duration{1 * time.Second, 3 * time.Second, 10 * time.Second},
		DrainEvery: 60 * time.Second,
		queue:      make(chan event.Event, 256),
	}, nil
}

// Enqueue は受信側から呼ぶ。送信の待ちで受信を止めないよう、キューに積むだけ。
// キューが溢れたら直接退避する(落とさない)。
func (d *Deliverer) Enqueue(e event.Event) {
	select {
	case d.queue <- e:
	default:
		d.log.Warn("queue full; spooling", "type", e.Type, "event_id", e.ID)
		d.spool(e)
	}
}

// Run はキューを捌き続ける。ctx が終わったら、残りを退避してから返る。
func (d *Deliverer) Run(ctx context.Context) {
	ticker := time.NewTicker(d.DrainEvery)
	defer ticker.Stop()

	d.Drain(ctx)
	for {
		select {
		case <-ctx.Done():
			d.flushQueueToSpool()
			return
		case e := <-d.queue:
			// 退避ぶんが残っているなら先に送る。追い越すと、取消が受付より先に届く。
			if d.SpoolCount() > 0 && d.Drain(ctx) > 0 {
				d.spool(e)
				continue
			}
			d.deliver(ctx, e)
		case <-ticker.C:
			d.Drain(ctx)
		}
	}
}

// SpoolCount は退避中の件数。
func (d *Deliverer) SpoolCount() int {
	return len(d.spoolFiles())
}

var errPermanent = errors.New("permanent")

func (d *Deliverer) deliver(ctx context.Context, e event.Event) {
	for attempt := 0; ; attempt++ {
		err := d.post(ctx, e)
		if err == nil {
			d.log.Info("delivered", "type", e.Type, "patient_number", e.PatientNumber, "event_id", e.ID)
			return
		}
		if errors.Is(err, errPermanent) {
			// 形が悪いイベントは何度送っても通らない。退避すると後続が詰まるので捨てる。
			d.log.Error("rejected; dropped", "type", e.Type, "event_id", e.ID, "error", err)
			return
		}
		if attempt >= len(d.Backoff) || ctx.Err() != nil {
			d.log.Error("undeliverable; spooled", "type", e.Type, "event_id", e.ID, "error", err)
			d.spool(e)
			return
		}
		d.log.Warn("retrying", "type", e.Type, "event_id", e.ID, "in", d.Backoff[attempt].String(), "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(d.Backoff[attempt]):
		}
	}
}

func (d *Deliverer) post(ctx context.Context, e event.Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.token)

	res, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 300))

	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusBadRequest, res.StatusCode == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: HTTP %d %s", errPermanent, res.StatusCode, snippet)
	default:
		// 401/403 はトークンやホスト許可の設定ミス。直せば通るので、捨てずに待つ。
		return fmt.Errorf("HTTP %d %s", res.StatusCode, snippet)
	}
}

// Drain は退避ぶんを古い順に送り直す。1 件でも送れなければそこで止める
// (順序を保つため)。残った件数を返す。
func (d *Deliverer) Drain(ctx context.Context) int {
	files := d.spoolFiles()
	for i, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var e event.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			d.log.Error("unreadable spool file; removed", "file", filepath.Base(path))
			os.Remove(path)
			continue
		}
		if err := d.post(ctx, e); err != nil && !errors.Is(err, errPermanent) {
			return len(files) - i
		}
		os.Remove(path)
		d.log.Info("resent from spool", "type", e.Type, "event_id", e.ID)
	}
	return 0
}

func (d *Deliverer) spool(e event.Event) {
	d.mu.Lock()
	d.seq++
	// 時刻 + 連番で、同じミリ秒に積まれても順序が崩れないようにする。
	name := fmt.Sprintf("%s-%06d.json", time.Now().UTC().Format("20060102T150405.000"), d.seq)
	d.mu.Unlock()

	body, _ := json.Marshal(e)
	tmp := filepath.Join(d.spoolDir, name+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		d.log.Error("spool write failed; event lost", "type", e.Type, "event_id", e.ID, "error", err)
		return
	}
	// 書きかけのファイルを Drain が拾わないよう、rename で確定させる。
	os.Rename(tmp, filepath.Join(d.spoolDir, name))
}

func (d *Deliverer) spoolFiles() []string {
	files, _ := filepath.Glob(filepath.Join(d.spoolDir, "*.json"))
	sort.Strings(files)
	return files
}

func (d *Deliverer) flushQueueToSpool() {
	for {
		select {
		case e := <-d.queue:
			d.spool(e)
		default:
			return
		}
	}
}
