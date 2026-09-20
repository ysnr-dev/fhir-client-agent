package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ysnr-dev/fhir-client-agent/internal/deliver"
	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

type scriptedAdapter struct {
	runs atomic.Int32
	run  func(n int32, ctx context.Context, emit func(event.Event), ready func()) error
}

func (s *scriptedAdapter) Name() string { return "scripted" }
func (s *scriptedAdapter) Run(ctx context.Context, emit func(event.Event), ready func()) error {
	return s.run(s.runs.Add(1), ctx, emit, ready)
}

func newAgent(t *testing.T, adapter Adapter, url string) *Agent {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := deliver.New(url, "t", t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	a := New(adapter, d, log)
	a.MinBackoff, a.MaxBackoff = time.Millisecond, 4*time.Millisecond
	return a
}

// 切れたら繋ぎ直し、繋がっているあいだに来たイベントをカルテへ届ける。
func TestRunReconnectsAndDelivers(t *testing.T) {
	var delivered atomic.Int32
	karte := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer karte.Close()

	ctx, cancel := context.WithCancel(context.Background())
	adapter := &scriptedAdapter{}
	adapter.run = func(n int32, ctx context.Context, emit func(event.Event), ready func()) error {
		switch n {
		case 1:
			return errors.New("connection refused") // 連携先がまだ起きていない
		case 2:
			ready()
			emit(event.Event{ID: "a", Type: event.PatientChanged, PatientNumber: "1"})
			return errors.New("closed") // 連携先の再起動
		default:
			ready()
			<-ctx.Done()
			return nil
		}
	}
	a := newAgent(t, adapter, karte.URL)

	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for (adapter.runs.Load() < 3 || delivered.Load() < 1) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !a.Status().Connected {
		t.Error("status should be connected on the third run")
	}
	cancel()
	<-done

	if adapter.runs.Load() != 3 || delivered.Load() != 1 {
		t.Errorf("runs %d delivered %d", adapter.runs.Load(), delivered.Load())
	}
	if a.Status().Connected {
		t.Error("status should be disconnected after shutdown")
	}
}

// 監視はステータスコードだけ見ればよい。
func TestHealthReports503WhileDisconnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := &scriptedAdapter{run: func(int32, context.Context, func(event.Event), func()) error { return nil }}
	a := newAgent(t, adapter, "http://127.0.0.1:1")
	if err := a.ServeHealth(ctx, "127.0.0.1:18787"); err != nil {
		t.Skip("port in use:", err)
	}

	res, err := http.Get("http://127.0.0.1:18787/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", res.StatusCode)
	}

	a.setConnected(true)
	res, _ = http.Get("http://127.0.0.1:18787/healthz")
	if res.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", res.StatusCode)
	}
}
