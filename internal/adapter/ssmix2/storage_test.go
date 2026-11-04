package ssmix2

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func message(process, txn string) Message {
	return Message{
		ID: 1, PatientID: "0001234", ServiceDate: "20261104", DataKind: "OMP-01", OrderNo: "000000000000045",
		Process: process, DepartmentCode: "01", TransactionAt: txn,
		BodyBase64: base64.StdEncoding.EncodeToString([]byte("MSH|^~\\&|HIS|\r")),
	}
}

func names(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// 登録 → 修正 → 取消 で、有効(1)・過去履歴(2)・無効(0)の付け替えが規則どおりになる。
func TestWriteKeepsHistoryAndCancels(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "000", "123", "0001234", "20261104", "OMP-01")

	if _, err := Write(root, message("INS", "20261104100000001")); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); len(got) != 1 || got[0] != "0001234_20261104_OMP-01_000000000000045_20261104100000001_01_1" {
		t.Fatalf("after first INS: %v", got)
	}

	if _, err := Write(root, message("INS", "20261104100500002")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0001234_20261104_OMP-01_000000000000045_20261104100000001_01_2",
		"0001234_20261104_OMP-01_000000000000045_20261104100500002_01_1",
	}
	if got := names(t, dir); !equal(got, want) {
		t.Fatalf("after second INS: %v", got)
	}

	if _, err := Write(root, message("DEL", "20261104101000003")); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"0001234_20261104_OMP-01_000000000000045_20261104100000001_01_0",
		"0001234_20261104_OMP-01_000000000000045_20261104100500002_01_0",
		"0001234_20261104_OMP-01_000000000000045_20261104101000003_01_0",
	}
	if got := names(t, dir); !equal(got, want) {
		t.Fatalf("after DEL: %v", got)
	}

	body, _ := os.ReadFile(filepath.Join(dir, want[2]))
	if string(body) != "MSH|^~\\&|HIS|\r" {
		t.Errorf("body = %q", body)
	}
}

// 同じメッセージが配り直されても、既にあるファイルを見て何もしない(履歴をずらさない)。
func TestWriteIsIdempotent(t *testing.T) {
	root := t.TempDir()
	m := message("INS", "20261104100000001")
	if _, err := Write(root, m); err != nil {
		t.Fatal(err)
	}
	out, err := Write(root, m)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Existed {
		t.Error("second write should report the existing file")
	}
	dir := filepath.Join(root, "000", "123", "0001234", "20261104", "OMP-01")
	if got := names(t, dir); len(got) != 1 || got[0] != "0001234_20261104_OMP-01_000000000000045_20261104100000001_01_1" {
		t.Errorf("files = %v", got)
	}
}

// 別のオーダや別の種別のファイルは改名しない。
func TestWriteLeavesOtherOrdersAlone(t *testing.T) {
	root := t.TempDir()
	other := message("INS", "20261104090000000")
	other.OrderNo = "000000000000044"
	if _, err := Write(root, other); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(root, message("DEL", "20261104100000001")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "000", "123", "0001234", "20261104", "OMP-01")
	want := []string{
		"0001234_20261104_OMP-01_000000000000044_20261104090000000_01_1",
		"0001234_20261104_OMP-01_000000000000045_20261104100000001_01_0",
	}
	if got := names(t, dir); !equal(got, want) {
		t.Errorf("files = %v", got)
	}
}

// 患者基本情報は診療日 "-" のフォルダに入る。
func TestWriteUndated(t *testing.T) {
	root := t.TempDir()
	m := message("INS", "20261104100000001")
	m.ServiceDate, m.DataKind, m.OrderNo, m.DepartmentCode = "-", "ADT-00", "ALL9", "-"
	out, err := Write(root, m)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "000", "123", "0001234", "-", "ADT-00", "0001234_-_ADT-00_ALL9_20261104100000001_-_1")
	if out.Path != want {
		t.Errorf("path = %s, want %s", out.Path, want)
	}
}

func TestWriteRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	cases := map[string]func(*Message){
		"underscore": func(m *Message) { m.OrderNo = "12_3" },
		"separator":  func(m *Message) { m.DataKind = "../x" },
		"short id":   func(m *Message) { m.PatientID = "123" },
		"process":    func(m *Message) { m.Process = "UPD" },
		"empty":      func(m *Message) { m.DepartmentCode = "" },
		"bad base64": func(m *Message) { m.BodyBase64 = "%%%" },
		"empty body": func(m *Message) { m.BodyBase64 = "" },
	}
	for name, mutate := range cases {
		m := message("INS", "20261104100000001")
		mutate(&m)
		if _, err := Write(root, m); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("nothing should have been written: %v", entries)
	}
}

func TestCheckRoot(t *testing.T) {
	if err := checkRoot("", ""); err == nil {
		t.Error("empty root should be rejected")
	}
	if err := checkRoot("relative/dir", ""); err == nil {
		t.Error("relative root should be rejected")
	}
	if err := checkRoot("/mnt/ssmix2/2219999998", "/mnt/ssmix2"); err != nil {
		t.Error(err)
	}
	if err := checkRoot("/mnt/ssmix2", "/mnt/ssmix2"); err != nil {
		t.Error(err)
	}
	if err := checkRoot("/mnt/other", "/mnt/ssmix2"); err == nil {
		t.Error("root outside the allowed root should be rejected")
	}
	if err := checkRoot("/mnt/ssmix2/../other", "/mnt/ssmix2"); err == nil {
		t.Error("root escaping with .. should be rejected")
	}
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
