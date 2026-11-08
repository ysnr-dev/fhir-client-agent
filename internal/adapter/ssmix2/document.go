package ssmix2

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// StorageExtended はメッセージの storage の値で、拡張ストレージ(診療文書)に置くもの。空か "standard" は標準化ストレージ。
const StorageExtended = "extended"

// 主文書のファイル名をカルテが言わなかったときの既定。
const defaultDocumentFileName = "HL7CDA.xml"

// データ種別フォルダ名の長さの上限(構築ガイドライン 2.2(4)③ 3))。
const maxDocumentFolderLength = 180

// File はコンテンツフォルダに置く主文書以外のファイル(コンテンツ定義ファイルなど)。
type File struct {
	Name       string `json:"name"`
	BodyBase64 string `json:"body_base64"`
}

// WriteDocument は診療文書を拡張ストレージの規則(構築ガイドライン 2.2)で置く。
//
//	root / 患者ID先頭3 / 患者ID次の3 / 患者ID / 診療日 / データ種別フォルダ /
//	患者ID_診療日_データ種別コード_特定キー_発生日時_診療科_コンディションフラグ / ファイル群
//
// データ種別フォルダは「^ローカル文書名称^^標準文書コード^標準文書名称^LN」のようにカルテが決めた名前そのまま。
// INS は同じ特定キーの有効(_1)フォルダを過去履歴(_2)に改名してから、新しい _1 フォルダに主文書と添付をすべて置く。
// DEL は有効(_1)と過去履歴(_2)を無効(_0)に改名するだけで、新しいフォルダは作らない(2.2(5)⑥)。
// 同じ名前(フラグ除く)のフォルダが既にあれば、配り直しとみなして何もしない。
func WriteDocument(root string, m Message) (Outcome, error) {
	if err := validate(m); err != nil {
		return Outcome{}, err
	}
	if err := validateDocument(m); err != nil {
		return Outcome{}, err
	}
	body, err := base64.StdEncoding.DecodeString(m.BodyBase64)
	if err != nil {
		return Outcome{}, fmt.Errorf("body: %w", err)
	}
	if len(body) == 0 {
		return Outcome{}, errors.New("body is empty")
	}

	cleanRoot := filepath.Clean(root)
	kindDir := filepath.Join(cleanRoot, m.PatientID[:3], m.PatientID[3:6], m.PatientID, m.ServiceDate, m.DocumentFolder)
	if !within(kindDir, cleanRoot) {
		return Outcome{}, fmt.Errorf("path escapes the root: %s", kindDir)
	}
	stem := strings.Join([]string{m.PatientID, m.ServiceDate, m.DataKind, m.OrderNo, m.TransactionAt, m.DepartmentCode}, "_")
	prefix := strings.Join([]string{m.PatientID, m.ServiceDate, m.DataKind, m.OrderNo}, "_") + "_"

	if existing := existingFolders(kindDir, stem); len(existing) > 0 {
		return Outcome{Path: existing[0], Existed: true, Body: body}, nil
	}
	if err := os.MkdirAll(kindDir, 0o755); err != nil {
		return Outcome{}, fmt.Errorf("mkdir: %w", err)
	}

	if m.Process == "DEL" {
		if err := renameFlags(kindDir, prefix, []string{"1", "2"}, "0", true); err != nil {
			return Outcome{}, err
		}
		return Outcome{Path: filepath.Join(kindDir, stem+"_0"), Body: body}, nil
	}

	if err := renameFlags(kindDir, prefix, []string{"1"}, "2", true); err != nil {
		return Outcome{}, err
	}
	final := filepath.Join(kindDir, stem+"_1")
	tmp := final + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return Outcome{}, fmt.Errorf("clean tmp: %w", err)
	}
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return Outcome{}, fmt.Errorf("mkdir: %w", err)
	}
	if err := writeFiles(tmp, m, body); err != nil {
		os.RemoveAll(tmp)
		return Outcome{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return Outcome{}, fmt.Errorf("rename: %w", err)
	}
	return Outcome{Path: final, Body: body}, nil
}

func writeFiles(dir string, m Message, body []byte) error {
	name := m.FileName
	if name == "" {
		name = defaultDocumentFileName
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	for _, f := range m.Files {
		content, err := base64.StdEncoding.DecodeString(f.BodyBase64)
		if err != nil {
			return fmt.Errorf("file %s: %w", f.Name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.Name, err)
		}
	}
	return nil
}

// existingFolders は stem で始まるコンテンツフォルダ(書きかけの .tmp は除く)。
func existingFolders(dir, stem string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, stem+"_*"))
	var out []string
	for _, match := range matches {
		if strings.HasSuffix(match, ".tmp") {
			continue
		}
		if info, err := os.Stat(match); err == nil && info.IsDir() {
			out = append(out, match)
		}
	}
	return out
}

// validateDocument は拡張ストレージだけが持つ項目を見る。
func validateDocument(m Message) error {
	folder := m.DocumentFolder
	if folder == "" {
		return errors.New("document_folder is empty")
	}
	if strings.ContainsAny(folder, "_/\\") || folder == "." || folder == ".." {
		return fmt.Errorf("document_folder contains a character that is not allowed in a folder name: %q", folder)
	}
	if utf8.RuneCountInString(folder) > maxDocumentFolderLength {
		return fmt.Errorf("document_folder is longer than %d characters", maxDocumentFolderLength)
	}
	if strings.Count(folder, "^") != 5 {
		return fmt.Errorf("document_folder must have 6 components separated by ^: %q", folder)
	}
	names := []string{m.FileName}
	for _, f := range m.Files {
		names = append(names, f.Name)
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return fmt.Errorf("file name is not allowed: %q", name)
		}
	}
	return nil
}
