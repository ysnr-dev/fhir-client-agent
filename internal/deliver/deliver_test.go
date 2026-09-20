package deliver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// karte は受け取ったイベントを覚え、決めた順にステータスを返す偽のカルテ。
type karte struct {
	mu       sync.Mutex
	statuses []int
	received []event.Event
	auth     []string
	server   *httptest.Server
}

func newKarte(t *testing.T, statuses ...int) *karte {
	k := &karte{statuses: statuses}
	k.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		status := http.StatusAccepted
		if len(k.statuses) > 0 {
			status, k.statuses = k.statuses[0], k.statuses[1:]
		}
		if status < 300 {
			var e event.Event
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &e)
			k.received = append(k.received, e)
		}
		k.auth = append(k.auth, r.Header.Get("Authorization"))
		w.WriteHeader(status)
	}))
	t.Cleanup(k.server.Close)
	return k
}

func (k *karte) ids() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	ids := []string{}
	for _, e := range k.received {
		ids = append(ids, e.ID)
	}
	return ids
}

func newDeliverer(t *testing.T, url string) *Deliverer {
	d, err := New(url, "token-1", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	return d
}

func ev(id string) event.Event {
	return event.Event{ID: id, Type: event.PatientChanged, PatientNumber: "00002"}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDeliverSendsBearerToken(t *testing.T) {
	k := newKarte(t)
	d := newDeliverer(t, k.server.URL)

	d.deliver(context.Background(), ev("a"))

	if !equal(k.ids(), []string{"a"}) || k.auth[0] != "Bearer token-1" {
		t.Errorf("received %v auth %v", k.ids(), k.auth)
	}
}

// 一時的な失敗は送り直して届ける。
func TestDeliverRetriesTransientFailures(t *testing.T) {
	k := newKarte(t, 503, 503, 202)
	d := newDeliverer(t, k.server.URL)

	d.deliver(context.Background(), ev("a"))

	if !equal(k.ids(), []string{"a"}) || d.SpoolCount() != 0 {
		t.Errorf("received %v spooled %d", k.ids(), d.SpoolCount())
	}
}

// 通知は再配達されないので、送れなかったものは落とさず退避する。
func TestDeliverSpoolsWhenRetriesRunOut(t *testing.T) {
	k := newKarte(t, 503, 503, 503)
	d := newDeliverer(t, k.server.URL)

	d.deliver(context.Background(), ev("a"))

	if d.SpoolCount() != 1 {
		t.Fatalf("spooled %d, want 1", d.SpoolCount())
	}
	if remaining := d.Drain(context.Background()); remaining != 0 || !equal(k.ids(), []string{"a"}) {
		t.Errorf("after drain: remaining %d received %v", remaining, k.ids())
	}
}

// 401 はトークンの設定ミス。直せば通るので、捨てずに待つ。
func TestDeliverKeepsEventsRejectedForAuth(t *testing.T) {
	k := newKarte(t, 401, 401, 401)
	d := newDeliverer(t, k.server.URL)

	d.deliver(context.Background(), ev("a"))

	if d.SpoolCount() != 1 {
		t.Errorf("spooled %d, want 1", d.SpoolCount())
	}
}

// 形が悪いイベントは何度送っても通らない。退避すると後続が詰まるので捨てる。
func TestDeliverDropsMalformedEvents(t *testing.T) {
	k := newKarte(t, 400)
	d := newDeliverer(t, k.server.URL)

	d.deliver(context.Background(), ev("a"))

	if d.SpoolCount() != 0 || len(k.auth) != 1 {
		t.Errorf("spooled %d after %d requests", d.SpoolCount(), len(k.auth))
	}
}

// 追い越すと、取消が受付より先に届く。
func TestDrainKeepsOrderAndStopsAtFirstFailure(t *testing.T) {
	k := newKarte(t)
	d := newDeliverer(t, k.server.URL)
	d.spool(ev("a"))
	d.spool(ev("b"))
	d.spool(ev("c"))

	k.statuses = []int{202, 503}
	if remaining := d.Drain(context.Background()); remaining != 2 {
		t.Fatalf("remaining %d, want 2", remaining)
	}
	if remaining := d.Drain(context.Background()); remaining != 0 {
		t.Fatalf("remaining %d, want 0", remaining)
	}
	if !equal(k.ids(), []string{"a", "b", "c"}) {
		t.Errorf("order %v", k.ids())
	}
}

// 退避ぶんが残っているあいだに来たイベントは、その後ろに並べる。
func TestRunQueuesBehindSpooledEvents(t *testing.T) {
	k := newKarte(t)
	d := newDeliverer(t, k.server.URL)
	d.spool(ev("old"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	d.Enqueue(ev("new"))

	deadline := time.Now().Add(2 * time.Second)
	for len(k.ids()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if !equal(k.ids(), []string{"old", "new"}) {
		t.Errorf("order %v", k.ids())
	}
}

// 終了時にキューに残っていたものは退避する(SIGTERM で通知を失わない)。
func TestRunSpoolsQueuedEventsOnShutdown(t *testing.T) {
	d := newDeliverer(t, "http://127.0.0.1:1") // 繋がらない
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Enqueue(ev("a"))
	d.Enqueue(ev("b"))

	d.Run(ctx)

	if d.SpoolCount() != 2 {
		t.Errorf("spooled %d, want 2", d.SpoolCount())
	}
}
