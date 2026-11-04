// Package output writes generated files, terminal previews and progress.
package output

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/lassejlv/ai-cli/internal/media"
)

// IsTTY reports whether f is a terminal.
func IsTTY(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

var unsafeStem = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Stem sanitizes a response id into a filename stem, or returns a random one.
func Stem(id string) string {
	s := unsafeStem.ReplaceAllString(strings.TrimSpace(id), "-")
	s = strings.Trim(s, "-")
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" || s == "." || s == ".." {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
	return s
}

// IsDirTarget reports whether p names a directory (existing or trailing slash).
func IsDirTarget(p string) bool {
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, string(os.PathSeparator)) {
		return true
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// AddSuffix inserts -suffix before the file extension.
func AddSuffix(name, suffix string) string {
	if suffix == "" {
		return name
	}
	ext := filepath.Ext(name)
	if ext == "" || strings.Contains(ext, string(os.PathSeparator)) {
		return name + "-" + suffix
	}
	return strings.TrimSuffix(name, ext) + "-" + suffix + ext
}

// Target computes where an artifact is saved.
func Target(outputPath, id, mediaType, suffix string) string {
	name := AddSuffix(Stem(id)+media.Extension(mediaType), suffix)
	if outputPath == "" {
		return name
	}
	if IsDirTarget(outputPath) {
		return filepath.Join(outputPath, name)
	}
	return AddSuffix(outputPath, suffix)
}

// Save writes data and returns the absolute path.
func Save(path string, data []byte) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return "", err
	}
	return abs, nil
}
