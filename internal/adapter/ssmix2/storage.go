package ssmix2

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Outcome は 1 件の書き込みの結果。Existed は同じファイルが既にあって何もしなかったこと。
// Body は置いた電文(トランザクションストレージへの追記に使う)。
type Outcome struct {
	Path    string
	Existed bool
	Body    []byte
}

// Write はメッセージを標準化ストレージの規則で置く。
//
//	root / 患者ID先頭3 / 患者ID次の3 / 患者ID / 診療日("-"も) / データ種別 /
//	患者ID_診療日_データ種別_オーダNo_トランザクション日時_診療科_コンディションフラグ
//
// INS は同じオーダの有効(_1)を過去履歴(_2)に改名してから _1 で書く。
// DEL は有効(_1)と過去履歴(_2)を無効(_0)に改名し、取消の電文を _0 で書く。
// 同じファイル名(フラグ除く)が既にあれば、配り直しとみなして何もしない。
func Write(root string, m Message) (Outcome, error) {
	if err := validate(m); err != nil {
		return Outcome{}, err
	}
	body, err := base64.StdEncoding.DecodeString(m.BodyBase64)
	if err != nil {
		return Outcome{}, fmt.Errorf("body: %w", err)
	}
	if len(body) == 0 {
		return Outcome{}, errors.New("body is empty")
	}

	dir, err := targetDir(root, m)
	if err != nil {
		return Outcome{}, err
	}
	stem := strings.Join([]string{m.PatientID, m.ServiceDate, m.DataKind, m.OrderNo, m.TransactionAt, m.DepartmentCode}, "_")
	flag := "1"
	if m.Process == "DEL" {
		flag = "0"
	}
	final := filepath.Join(dir, stem+"_"+flag)

	if existing, _ := filepath.Glob(filepath.Join(dir, stem+"_*")); len(existing) > 0 {
		return Outcome{Path: existing[0], Existed: true, Body: body}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Outcome{}, fmt.Errorf("mkdir: %w", err)
	}

	prefix := strings.Join([]string{m.PatientID, m.ServiceDate, m.DataKind, m.OrderNo}, "_") + "_"
	if m.Process == "DEL" {
		if err := renameFlags(dir, prefix, []string{"1", "2"}, "0"); err != nil {
			return Outcome{}, err
		}
	} else {
		if err := renameFlags(dir, prefix, []string{"1"}, "2"); err != nil {
			return Outcome{}, err
		}
	}

	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return Outcome{}, fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return Outcome{}, fmt.Errorf("rename: %w", err)
	}
	return Outcome{Path: final, Body: body}, nil
}

// renameFlags は prefix で始まり末尾のフラグが from のいずれかのファイルを to に改名する。
// ファイル名は 7 項目で、prefix より後ろは "日時_診療科_フラグ"。
func renameFlags(dir, prefix string, from []string, to string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("readdir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, ".tmp") {
			continue
		}
		rest := strings.Split(strings.TrimPrefix(name, prefix), "_")
		if len(rest) != 3 {
			continue
		}
		matched := false
		for _, f := range from {
			if rest[2] == f {
				matched = true
			}
		}
		if !matched {
			continue
		}
		renamed := prefix + rest[0] + "_" + rest[1] + "_" + to
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, renamed)); err != nil {
			return fmt.Errorf("rename %s: %w", name, err)
		}
	}
	return nil
}

func targetDir(root string, m Message) (string, error) {
	cleanRoot := filepath.Clean(root)
	dir := filepath.Join(cleanRoot, m.PatientID[:3], m.PatientID[3:6], m.PatientID, m.ServiceDate, m.DataKind)
	if !within(dir, cleanRoot) {
		return "", fmt.Errorf("path escapes the root: %s", dir)
	}
	return dir, nil
}

// within は path が root の下か。root そのものは含まない。
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkRoot はカルテが指定した出力先を受け入れるか。絶対パスで、許可ルートがあればその下に限る。
func checkRoot(root, allowed string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("output root is empty")
	}
	if !filepath.IsAbs(root) && !strings.HasPrefix(root, `\\`) {
		return fmt.Errorf("output root must be absolute: %q", root)
	}
	if allowed == "" {
		return nil
	}
	cleanRoot, cleanAllowed := filepath.Clean(root), filepath.Clean(allowed)
	if cleanRoot == cleanAllowed || within(cleanRoot, cleanAllowed) {
		return nil
	}
	return fmt.Errorf("%w: %q", errRootNotAllowed, root)
}

// validate はファイル名とフォルダ名に使う項目を見る。"_" とパス区切りを含めない、空にしない。
func validate(m Message) error {
	parts := map[string]string{
		"patient_id": m.PatientID, "service_date": m.ServiceDate, "data_kind": m.DataKind,
		"order_no": m.OrderNo, "transaction_at": m.TransactionAt, "department_code": m.DepartmentCode,
	}
	for name, value := range parts {
		if value == "" {
			return fmt.Errorf("%s is empty", name)
		}
		if strings.ContainsAny(value, "_/\\") || value == "." || value == ".." {
			return fmt.Errorf("%s contains a character that is not allowed in a file name: %q", name, value)
		}
	}
	if len(m.PatientID) < 6 {
		return fmt.Errorf("patient_id must be at least 6 characters: %q", m.PatientID)
	}
	if m.Process != "INS" && m.Process != "DEL" {
		return fmt.Errorf("process must be INS or DEL: %q", m.Process)
	}
	return nil
}
