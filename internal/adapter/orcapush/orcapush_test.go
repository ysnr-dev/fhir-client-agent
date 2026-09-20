package orcapush

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ysnr-dev/fhir-client-agent/internal/config"
	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// fakeOrca は購読を受けて、決めたメッセージを流す偽の日レセ。
type fakeOrca struct {
	tenant    chan string
	subscribe chan frame
	script    func(ctx context.Context, conn *websocket.Conn)
}

func (f *fakeOrca) start(t *testing.T) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.tenant <- r.Header.Get("X-GINBEE-TENANT-ID")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var sub frame
		json.Unmarshal(raw, &sub)
		f.subscribe <- sub
		f.script(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
}

func send(ctx context.Context, conn *websocket.Conn, v any) {
	raw, _ := json.Marshal(v)
	conn.Write(ctx, websocket.MessageText, raw)
}

func newAdapter(t *testing.T, url string) *Adapter {
	a, err := New(config.OrcaPush{WSURL: url, TenantID: "1"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRunSubscribesAndForwardsEvents(t *testing.T) {
	orca := &fakeOrca{tenant: make(chan string, 1), subscribe: make(chan frame, 1)}
	orca.script = func(ctx context.Context, conn *websocket.Conn) {
		send(ctx, conn, map[string]any{"command": "subscribed", "sub.id": "s-1"})
		send(ctx, conn, map[string]any{"command": "event", "sub.id": "s-1", "data": map[string]any{
			"uuid": "u-1", "event": "patient_information", "time": "2026-09-20T10:15:00+09:00",
			"body": map[string]any{"Patient_Mode": "modify", "Patient_ID": "00002"},
		}})
		// カルテが使わないイベント。流れてきても転送しない。
		send(ctx, conn, map[string]any{"command": "event", "sub.id": "s-1", "data": map[string]any{
			"uuid": "u-2", "event": "patient_account", "body": map[string]any{"Patient_Mode": "add"},
		}})
		conn.Close(websocket.StatusNormalClosure, "")
	}
	adapter := newAdapter(t, orca.start(t))

	var got []event.Event
	ready := 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := adapter.Run(ctx, func(e event.Event) { got = append(got, e) }, func() { ready++ })

	// オンプレは認証が無く、テナントはクライアントが名乗る。
	if tenant := <-orca.tenant; tenant != "1" {
		t.Errorf("X-GINBEE-TENANT-ID = %q", tenant)
	}
	if sub := <-orca.subscribe; sub.Command != "subscribe" || sub.Event != "*" || sub.ReqID == "" {
		t.Errorf("subscribe frame = %+v", sub)
	}
	if ready != 1 {
		t.Errorf("ready called %d times", ready)
	}
	if len(got) != 1 || got[0].ID != "u-1" || got[0].Type != event.PatientChanged {
		t.Errorf("forwarded %+v", got)
	}
	// 相手から切られたら、土台が再接続できるよう error で戻る。
	if err == nil {
		t.Error("Run should return an error when the server closes the connection")
	}
}

// 購読を断られたら、繋がったことにしない。
func TestRunFailsWhenSubscribeIsRejected(t *testing.T) {
	orca := &fakeOrca{tenant: make(chan string, 1), subscribe: make(chan frame, 1)}
	orca.script = func(ctx context.Context, conn *websocket.Conn) {
		send(ctx, conn, map[string]any{"command": "error", "for": "subscribe", "code": "INVALID_PARAMS", "reason": "bad"})
		time.Sleep(100 * time.Millisecond)
	}
	adapter := newAdapter(t, orca.start(t))

	ready := 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := adapter.Run(ctx, func(event.Event) {}, func() { ready++ })

	if err == nil || !strings.Contains(err.Error(), "INVALID_PARAMS") {
		t.Errorf("err = %v", err)
	}
	if ready != 0 {
		t.Error("ready must not be called")
	}
}

// 止めるよう言われたら、エラーにせず静かに戻る。
func TestRunReturnsNilOnShutdown(t *testing.T) {
	orca := &fakeOrca{tenant: make(chan string, 1), subscribe: make(chan frame, 1)}
	orca.script = func(ctx context.Context, conn *websocket.Conn) {
		send(ctx, conn, map[string]any{"command": "subscribed", "sub.id": "s-1"})
		<-ctx.Done()
	}
	adapter := newAdapter(t, orca.start(t))

	ctx, cancel := context.WithCancel(context.Background())
	err := adapter.Run(ctx, func(event.Event) {}, cancel)

	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}
