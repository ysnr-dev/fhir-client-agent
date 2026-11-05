package ssmix2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock(times ...time.Time) func() time.Time {
	i := 0
	return func() time.Time {
		if i < len(times)-1 {
			i++
			return times[i-1]
		}
		return times[len(times)-1]
	}
}

// 1 件ごとに「SS-MIX ヘッダー <EOH> HL7 <EOM>」を追記し、同じ日のうちは同じファイルに書く。
func TestTransactionLogAppendsHeaderAndMessage(t *testing.T) {
	dir := t.TempDir()
	log := NewTransactionLog(dir, "0001")
	at := time.Date(2026, 11, 5, 9, 30, 12, 345_000_000, time.Local)
	log.now = fixedClock(at, at.Add(time.Minute))
	defer log.Close()

	m := message("INS", "20261105093012345")
	if err := log.Append("2219999998", m, []byte("MSH|a\r")); err != nil {
		t.Fatal(err)
	}
	if err := log.Append("2219999998", message("DEL", "20261105093112000"), []byte("MSH|b\r")); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dir, "2026", "TR_20261105093012345_0001.DAT")
	if log.Path() != want {
		t.Fatalf("path = %q", log.Path())
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	records := strings.Split(string(body), "\x1c\r")
	if len(records) != 3 || records[2] != "" {
		t.Fatalf("records = %q", records)
	}
	if records[0] != "#SSMIX,2.00,2219999998,0001234,20261104,OMP-01,000000000000045,INS,01,20261105093012345\x1e\rMSH|a\r" {
		t.Errorf("record 1 = %q", records[0])
	}
	if !strings.HasPrefix(records[1], "#SSMIX,2.00,2219999998,0001234,20261104,OMP-01,000000000000045,DEL,01,") {
		t.Errorf("record 2 = %q", records[1])
	}
}

// 日付が変わるか上限に達したら新しいファイルに切り替える。
func TestTransactionLogRotates(t *testing.T) {
	dir := t.TempDir()
	log := NewTransactionLog(dir, "5678")
	day1 := time.Date(2026, 12, 31, 23, 59, 0, 0, time.Local)
	day2 := time.Date(2027, 1, 1, 0, 0, 1, 0, time.Local)
	log.now = fixedClock(day1, day2, day2.Add(time.Second))
	log.limit = 120
	defer log.Close()

	for i := 0; i < 3; i++ {
		if err := log.Append("2219999998", message("INS", "20261231235900000"), []byte("MSH|"+strings.Repeat("x", 20)+"\r")); err != nil {
			t.Fatal(err)
		}
	}
	if got := names(t, filepath.Join(dir, "2026")); len(got) != 1 || got[0] != "TR_20261231235900000_5678.DAT" {
		t.Errorf("2026 = %v", got)
	}
	// 年が変わって 1 本、上限を超えてもう 1 本。
	if got := names(t, filepath.Join(dir, "2027")); len(got) != 2 {
		t.Errorf("2027 = %v", got)
	}
}

// 置き場が空ならトランザクションストレージは持たず、Append は何もしない。
func TestTransactionLogDisabled(t *testing.T) {
	var log *TransactionLog = NewTransactionLog("", "0001")
	if log != nil {
		t.Fatal("expected nil")
	}
	if err := log.Append("x", message("INS", "1"), nil); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}
