package config

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExecutableDirEnv は、実環境変数を優先し、無ければ実行バイナリの隣の `.env` を
// 引く getenv を返す。院内サーバーへは「バイナリと .env を同じ場所に置く」だけで
// 配れるようにするため、カレントディレクトリではなくバイナリの場所を見る。
func ExecutableDirEnv(base func(string) string) func(string) string {
	path, err := dotenvPath()
	if err != nil {
		return base
	}
	f, err := os.Open(path)
	if err != nil {
		return base
	}
	defer f.Close()

	vals := parseDotenv(f)
	if len(vals) == 0 {
		return base
	}
	return func(key string) string {
		if v := base(key); v != "" {
			return v // 環境変数が常に勝つ(systemd の EnvironmentFile で上書きできる)
		}
		return vals[key]
	}
}

// ExecutableDir は実行バイナリの実体があるディレクトリ。symlink は辿る。
func ExecutableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

func dotenvPath() (string, error) {
	dir, err := ExecutableDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".env"), nil
}

// parseDotenv は KEY=VALUE の行だけを読む。変数展開はしない。
func parseDotenv(r io.Reader) map[string]string {
	vals := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key != "" {
			vals[key] = value
		}
	}
	return vals
}
