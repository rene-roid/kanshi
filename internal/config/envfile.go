package config

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the optional settings file. It uses the same KEY=VALUE format
// as a Compose .env file, so the two are interchangeable.
const FileName = "kanshi.env"

// findFile picks the settings file: an explicit path (flag, then
// KANSHI_CONFIG), else kanshi.env next to the executable, else one in the
// user's config directory. When none exists it still returns where one should
// be created, so the dashboard has somewhere to save an access change.
func findFile(explicit string) (path string, exists bool) {
	if explicit == "" {
		explicit = strings.TrimSpace(os.Getenv("KANSHI_CONFIG"))
	}
	if explicit != "" {
		return explicit, isFile(explicit)
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), FileName); isFile(p) {
			return p, true
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		// A scratch container has no HOME. There is nowhere to persist to,
		// and the access mode comes from the environment there anyway.
		return "", false
	}
	p := filepath.Join(dir, "kanshi", FileName)
	return p, isFile(p)
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// ReadFile parses a KEY=VALUE file. Blank lines and # comments are skipped, an
// "export " prefix is tolerated, and a value may be wrapped in single or double
// quotes.
func ReadFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, value, ok := parseLine(sc.Text())
		if ok {
			out[key] = value
		}
	}
	return out, sc.Err()
}

func parseLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, ok = strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	} else if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i]) // trailing comment on an unquoted value
	}
	return key, value, key != ""
}

// Writable reports whether SaveValue could write path: its directory can be
// created and written to. In a container with a read-only root filesystem it
// cannot, and the dashboard says so instead of offering to save.
func Writable(path string) bool {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".kanshi-*.tmp")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// SaveValue sets one key in the file, keeping every other line — comments
// included — exactly as it was. The file and its directory are created if
// needed, and the write goes through a rename so a crash cannot leave half a
// file behind.
func SaveValue(path, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	if len(raw) > 0 {
		lines = strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	}
	entry := key + "=" + value
	replaced := false
	for i, line := range lines {
		if k, _, ok := parseLine(strings.TrimRight(line, "\r")); ok && k == key {
			if !replaced {
				lines[i] = entry
				replaced = true
			} else {
				lines[i] = "# " + line // a later duplicate would win; neutralise it
			}
		}
	}
	if !replaced {
		lines = append(lines, entry)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kanshi-*.env")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
