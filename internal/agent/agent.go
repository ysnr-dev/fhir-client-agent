// Package agent は院内に常駐する連携プロセスの土台。
//
// 連携先ごとの違い(何に繋いで、何をイベントに直すか)は Adapter に閉じ、
// ここは常駐の作法 — 再接続、シグナル、送達、稼働状況 — だけを持つ。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ysnr-dev/fhir-client-agent/internal/deliver"
	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// Adapter は 1 つの連携先との接続。
type Adapter interface {
	Name() string
	// Run は接続して、切れるまで戻らない。接続が使える状態になったら ready を 1 回呼び、
	// 受けた通知は中立イベントに直して emit へ渡す。ctx が終わったら nil を返す。
	Run(ctx context.Context, emit func(event.Event), ready func()) error
}

// Agent は Adapter を動かし続ける。
type Agent struct {
	adapter   Adapter
	deliverer *deliver.Deliverer
	log       *slog.Logger

	// MinBackoff / MaxBackoff は再接続の待ち時間。失敗のたびに倍にし、繋がったら戻す。
	MinBackoff time.Duration
	MaxBackoff time.Duration

	mu          sync.Mutex
	connected   bool
	connectedAt time.Time
	lastEventAt time.Time
	events      int
}

func New(adapter Adapter, deliverer *deliver.Deliverer, log *slog.Logger) *Agent {
	return &Agent{
		adapter:    adapter,
		deliverer:  deliverer,
		log:        log,
		MinBackoff: 1 * time.Second,
		MaxBackoff: 30 * time.Second,
	}
}

// Run は ctx が終わるまで戻らない。終わるときは、未送信のイベントを退避してから返る。
func (a *Agent) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.deliverer.Run(ctx)
	}()

	backoff := a.MinBackoff
	for ctx.Err() == nil {
		established := false
		err := a.adapter.Run(ctx, a.emit, func() {
			established = true
			a.setConnected(true)
			a.log.Info("connected", "adapter", a.adapter.Name())
		})
		a.setConnected(false)
		if ctx.Err() != nil {
			break
		}

		// 一度でも繋がっていたなら、連携先の再起動などの一時的な切断。すぐ戻りに行く。
		if established {
			backoff = a.MinBackoff
		}
		wait := jitter(backoff)
		a.log.Warn("disconnected; reconnecting", "adapter", a.adapter.Name(), "in", wait.String(), "error", errString(err))

		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		backoff = min(backoff*2, a.MaxBackoff)
	}

	wg.Wait()
	a.log.Info("stopped", "adapter", a.adapter.Name(), "spooled", a.deliverer.SpoolCount())
}

func (a *Agent) emit(e event.Event) {
	a.mu.Lock()
	a.lastEventAt = time.Now()
	a.events++
	a.mu.Unlock()
	a.deliverer.Enqueue(e)
}

func (a *Agent) setConnected(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = v
	if v {
		a.connectedAt = time.Now()
	}
}

// Status は稼働状況。health エンドポイントと監視のため。
type Status struct {
	Adapter     string `json:"adapter"`
	Connected   bool   `json:"connected"`
	ConnectedAt string `json:"connected_at,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
	Events      int    `json:"events"`
	Spooled     int    `json:"spooled"`
}

func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Status{
		Adapter:   a.adapter.Name(),
		Connected: a.connected,
		Events:    a.events,
		Spooled:   a.deliverer.SpoolCount(),
	}
	if a.connected {
		s.ConnectedAt = a.connectedAt.Format(time.RFC3339)
	}
	if !a.lastEventAt.IsZero() {
		s.LastEventAt = a.lastEventAt.Format(time.RFC3339)
	}
	return s
}

// ServeHealth は addr で /healthz を待ち受ける。繋がっていなければ 503 を返すので、
// 監視はステータスコードだけ見ればよい。
func (a *Agent) ServeHealth(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		status := a.Status()
		w.Header().Set("Content-Type", "application/json")
		if !status.Connected {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(status)
	})

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("health server stopped", "error", err)
		}
	}()
	a.log.Info("health endpoint listening", "addr", listener.Addr().String())
	return nil
}

// jitter は ±20%。複数の院内機が同時に再接続して連携先を叩かないように。
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func errString(err error) string {
	if err == nil {
		return "connection closed"
	}
	return err.Error()
}
