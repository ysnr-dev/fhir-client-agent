package ssmix2

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SS-MIX ヘッダーの終了識別(0x1E 0x0D)と HL7 メッセージの終了識別(0x1C 0x0D)。
const (
	endOfHeader  = "\x1e\r"
	endOfMessage = "\x1c\r"
	ssmixVersion = "2.00"
)

// 1 つのトランザクションデータファイルに書く上限。超えたら次のファイルに切り替える。
const defaultTransactionFileLimit = 100 << 20

// TransactionLog はトランザクションストレージ(構築ガイドライン 3.3)。標準化ストレージに置いたメッセージを、
// 発生順に「SS-MIX ヘッダー + HL7 メッセージ」として 1 本のファイルに追記していく。
//
//	<dir>/<発生年>/TR_<書き始めた日時 YYYYMMDDHHMMSSFFF>_<処理系統番号>.DAT
//
// 日付が変わるか、ファイルが上限の大きさに達したら新しいファイルに切り替える。
type TransactionLog struct {
	dir    string
	lineNo string
	limit  int64
	now    func() time.Time

	mu       sync.Mutex
	file     *os.File
	path     string
	day      string
	size     int64
	disabled bool
}

// NewTransactionLog は dir が空なら nil(トランザクションストレージを持たない)。
func NewTransactionLog(dir, lineNo string) *TransactionLog {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &TransactionLog{dir: filepath.Clean(dir), lineNo: lineNo, limit: defaultTransactionFileLimit, now: time.Now}
}

// Append はメッセージ 1 件を追記する。body は標準化ストレージに置いた電文そのもの。
func (t *TransactionLog) Append(facilityID string, m Message, body []byte) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	header := strings.Join([]string{"#SSMIX", ssmixVersion, facilityID, m.PatientID, m.ServiceDate, m.DataKind,
		m.OrderNo, m.Process, m.DepartmentCode, m.TransactionAt}, ",")
	record := make([]byte, 0, len(header)+len(endOfHeader)+len(body)+len(endOfMessage))
	record = append(record, header...)
	record = append(record, endOfHeader...)
	record = append(record, body...)
	record = append(record, endOfMessage...)

	now := t.now()
	if err := t.rotate(now, int64(len(record))); err != nil {
		return err
	}
	n, err := t.file.Write(record)
	t.size += int64(n)
	if err != nil {
		return fmt.Errorf("transaction log: %w", err)
	}
	return nil
}

// rotate は書き込み先のファイルを用意する。開いていない・日付が変わった・上限に達するときは新しいファイル。
func (t *TransactionLog) rotate(now time.Time, next int64) error {
	day := now.Format("20060102")
	if t.file != nil && t.day == day && t.size+next <= t.limit {
		return nil
	}
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
	dir := filepath.Join(t.dir, now.Format("2006"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("transaction log: mkdir: %w", err)
	}
	stamp := now.Format("20060102150405") + fmt.Sprintf("%03d", now.Nanosecond()/int(time.Millisecond))
	path := filepath.Join(dir, "TR_"+stamp+"_"+t.lineNo+".DAT")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("transaction log: open: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("transaction log: stat: %w", err)
	}
	t.file, t.path, t.day, t.size = file, path, day, info.Size()
	return nil
}

// Path は今書いているファイル。まだ書いていなければ空。
func (t *TransactionLog) Path() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.path
}

// Close は書いているファイルを閉じる。
func (t *TransactionLog) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}
