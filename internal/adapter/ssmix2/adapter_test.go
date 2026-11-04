package ssmix2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ysnr-dev/fhir-client-agent/internal/config"
	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// 取りに行く → 書く → ack の 1 巡。2 回目のポーリングは空で、ack された id が記録されている。
func TestRunPollsWritesAndAcks(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	polls := 0
	var acked ackRequest
	var gotAuth string

	karte := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/integrations/ssmix2/messages":
			polls++
			res := response{Enabled: true, Storage: storage{RootPath: root, FacilityID: "2219999998"}}
			if polls == 1 {
				res.Messages = []Message{
					{ID: 11, PatientID: "0001234", ServiceDate: "-", DataKind: "ADT-00", OrderNo: "ALL9", Process: "INS",
						DepartmentCode: "-", TransactionAt: "20261104100000001",
						BodyBase64: base64.StdEncoding.EncodeToString([]byte("MSH|a\r"))},
					{ID: 12, PatientID: "0001234", ServiceDate: "20261104", DataKind: "OMP-01", OrderNo: "000000000000045", Process: "INS",
						DepartmentCode: "01", TransactionAt: "20261104100000002", BodyBase64: "%%%"},
				}
			}
			json.NewEncoder(w).Encode(res)
		case r.Method == http.MethodPost && r.URL.Path == "/integrations/ssmix2/messages/ack":
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &acked)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer karte.Close()

	a := New(config.Ssmix2Storage{MessagesURL: karte.URL + "/integrations/ssmix2/messages", PollInterval: 20 * time.Millisecond},
		"secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	readyCalls := 0
	if err := a.Run(ctx, func(event.Event) {}, func() { readyCalls++ }); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if readyCalls != 1 {
		t.Errorf("ready called %d times", readyCalls)
	}
	if polls < 2 {
		t.Errorf("polls = %d", polls)
	}
	if len(acked.Written) != 1 || acked.Written[0] != 11 || len(acked.Failed) != 1 || acked.Failed[0].ID != 12 {
		t.Errorf("ack = %+v", acked)
	}
	file := filepath.Join(root, "000", "123", "0001234", "-", "ADT-00", "0001234_-_ADT-00_ALL9_20261104100000001_-_1")
	if body, err := os.ReadFile(file); err != nil || string(body) != "MSH|a\r" {
		t.Errorf("file %s: %v %q", file, err, body)
	}
}

// 出力先が許可ルートの外なら書かず、ack もしない(カルテ側に残して直してもらう)。
func TestRunLeavesMessagesWhenRootIsNotAllowed(t *testing.T) {
	var acks int
	karte := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			acks++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(response{Enabled: true, Storage: storage{RootPath: "/somewhere/else"}, Messages: []Message{
			{ID: 1, PatientID: "0001234", ServiceDate: "-", DataKind: "ADT-00", OrderNo: "ALL9", Process: "INS",
				DepartmentCode: "-", TransactionAt: "20261104100000001", BodyBase64: base64.StdEncoding.EncodeToString([]byte("x"))},
		}})
	}))
	defer karte.Close()

	a := New(config.Ssmix2Storage{MessagesURL: karte.URL, PollInterval: 10 * time.Millisecond, AllowedRoot: t.TempDir()},
		"secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx, func(event.Event) {}, func() {}); err != nil {
		t.Fatal(err)
	}
	if acks != 0 {
		t.Errorf("acks = %d", acks)
	}
}

// カルテに繋がらなければエラーで返し、土台の再接続に任せる。
func TestRunReturnsErrorWhenKarteIsDown(t *testing.T) {
	a := New(config.Ssmix2Storage{MessagesURL: "http://127.0.0.1:1/integrations/ssmix2/messages", PollInterval: time.Second},
		"secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := a.Run(context.Background(), func(event.Event) {}, func() {}); err == nil {
		t.Error("expected an error")
	}
}
