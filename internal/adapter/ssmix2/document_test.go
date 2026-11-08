package ssmix2

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const noteFolder = "^経過記録^^11506-3^経過記録^LN"

func document(process, txn string) Message {
	return Message{
		ID: 1, Storage: StorageExtended, PatientID: "0001234", ServiceDate: "20261104", DataKind: "11506-3",
		OrderNo: "000000000000045", Process: process, DepartmentCode: "01", TransactionAt: txn,
		DocumentFolder: noteFolder, DocumentName: "経過記録", FileName: "HL7CDA.xml",
		BodyBase64: base64.StdEncoding.EncodeToString([]byte("<ClinicalDocument/>")),
		Files:      []File{{Name: "_contents.xml", BodyBase64: base64.StdEncoding.EncodeToString([]byte("<Contents/>"))}},
	}
}

// 登録 → 修正 → 削除 で、コンテンツフォルダの有効(1)・過去履歴(2)・無効(0)が付け替わり、削除では新しいフォルダを作らない。
func TestWriteDocumentKeepsHistoryAndCancels(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "000", "123", "0001234", "20261104", noteFolder)

	if _, err := WriteDocument(root, document("INS", "20261104100000001")); err != nil {
		t.Fatal(err)
	}
	first := "0001234_20261104_11506-3_000000000000045_20261104100000001_01_1"
	if got := names(t, dir); !equal(got, []string{first}) {
		t.Fatalf("after first INS: %v", got)
	}
	if body, err := os.ReadFile(filepath.Join(dir, first, "HL7CDA.xml")); err != nil || string(body) != "<ClinicalDocument/>" {
		t.Fatalf("main document: %v %q", err, body)
	}
	if body, err := os.ReadFile(filepath.Join(dir, first, "_contents.xml")); err != nil || string(body) != "<Contents/>" {
		t.Fatalf("contents file: %v %q", err, body)
	}

	if _, err := WriteDocument(root, document("INS", "20261104100500002")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0001234_20261104_11506-3_000000000000045_20261104100000001_01_2",
		"0001234_20261104_11506-3_000000000000045_20261104100500002_01_1",
	}
	if got := names(t, dir); !equal(got, want) {
		t.Fatalf("after second INS: %v", got)
	}

	outcome, err := WriteDocument(root, document("DEL", "20261104101000003"))
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"0001234_20261104_11506-3_000000000000045_20261104100000001_01_0",
		"0001234_20261104_11506-3_000000000000045_20261104100500002_01_0",
	}
	if got := names(t, dir); !equal(got, want) {
		t.Fatalf("after DEL: %v", got)
	}
	if outcome.Existed {
		t.Error("DEL reported as existed")
	}
}

// 同じフォルダ名(フラグ除く)があれば何もしない。書きかけの .tmp は無いものと数える。
func TestWriteDocumentIsIdempotent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "000", "123", "0001234", "20261104", noteFolder)
	if err := os.MkdirAll(filepath.Join(dir, "0001234_20261104_11506-3_000000000000045_20261104100000001_01_1.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := WriteDocument(root, document("INS", "20261104100000001"))
	if err != nil || first.Existed {
		t.Fatalf("first write: %+v %v", first, err)
	}
	again, err := WriteDocument(root, document("INS", "20261104100000001"))
	if err != nil || !again.Existed {
		t.Fatalf("second write: %+v %v", again, err)
	}
	if got := names(t, dir); !equal(got, []string{"0001234_20261104_11506-3_000000000000045_20261104100000001_01_1"}) {
		t.Fatalf("folders: %v", got)
	}
}

func TestWriteDocumentRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	cases := map[string]func(*Message){
		"folder with underscore": func(m *Message) { m.DocumentFolder = "^経過_記録^^11506-3^経過記録^LN" },
		"folder with separator":  func(m *Message) { m.DocumentFolder = "^経過/記録^^11506-3^経過記録^LN" },
		"folder components":      func(m *Message) { m.DocumentFolder = "11506-3" },
		"empty folder":           func(m *Message) { m.DocumentFolder = "" },
		"long folder":            func(m *Message) { m.DocumentFolder = "^" + strings.Repeat("あ", 180) + "^^11506-3^経過記録^LN" },
		"file name":              func(m *Message) { m.Files[0].Name = "../x" },
		"patient id":             func(m *Message) { m.PatientID = "00/1234" },
	}
	for name, mutate := range cases {
		m := document("INS", "20261104100000001")
		mutate(&m)
		if _, err := WriteDocument(root, m); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
