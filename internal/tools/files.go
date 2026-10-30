package tools

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// maxFileSize bounds the files that file tools read whole.
const maxFileSize = 20 << 20

// FileTracker remembers what each file looked like when the model last read
// or wrote it, so an edit can refuse to work from a stale view (SPEC §8.2).
type FileTracker struct {
	mu   sync.Mutex
	seen map[string]fileState
}

type fileState struct {
	size int64
	mod  time.Time
	sum  [sha256.Size]byte
}

func NewFileTracker() *FileTracker { return &FileTracker{seen: map[string]fileState{}} }

func trackKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// Record notes that the model has seen data as the content of path.
func (t *FileTracker) Record(path string, data []byte) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[trackKey(path)] = fileState{size: fi.Size(), mod: fi.ModTime(), sum: sha256.Sum256(data)}
}

// CheckUnchanged returns an error when path has changed since the model
// last read or wrote it. A file it has never seen passes.
func (t *FileTracker) CheckUnchanged(path, shown string) error {
	t.mu.Lock()
	st, ok := t.seen[trackKey(path)]
	t.mu.Unlock()
	if !ok {
		return nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s was deleted or moved since you last read it; check the path and read it again", shown)
	}
	if err != nil {
		return err
	}
	if fi.Size() == st.size && fi.ModTime().Equal(st.mod) {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256.Sum256(data) != st.sum {
		return fmt.Errorf("%s has changed since you last read it; read it again before changing it", shown)
	}
	t.Record(path, data) // touched, not changed
	return nil
}

// textFile is a file's text with its encoding details set apart, so it can
// be written back the way it was.
type textFile struct {
	text string // without a byte order mark, with LF line endings
	bom  bool
	crlf bool
}

var utf8BOM = []byte("\xef\xbb\xbf")

func decodeText(data []byte) textFile {
	f := textFile{}
	if bytes.HasPrefix(data, utf8BOM) {
		f.bom, data = true, data[len(utf8BOM):]
	}
	f.crlf = bytes.Contains(data, []byte("\r\n"))
	f.text = strings.ReplaceAll(string(data), "\r\n", "\n")
	return f
}

// encode turns text, with LF line endings, into file content with the
// file's line endings and byte order mark.
func (f textFile) encode(text string) []byte {
	if f.crlf {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if f.bom {
		return append(append([]byte(nil), utf8BOM...), text...)
	}
	return []byte(text)
}

// isBinary reports whether data looks binary: a NUL byte in its first 8 KB.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

// readText reads a text file for a tool.
func readText(path, shown string) (textFile, []byte, error) {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return textFile{}, nil, fmt.Errorf("%s does not exist", shown)
	case err != nil:
		return textFile{}, nil, err
	case fi.IsDir():
		return textFile{}, nil, fmt.Errorf("%s is a directory; use list_directory", shown)
	case fi.Size() > maxFileSize:
		return textFile{}, nil, fmt.Errorf("%s is too large (%s); search it with grep instead", shown, sizeText(fi.Size()))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return textFile{}, nil, err
	}
	if isBinary(data) {
		return textFile{}, nil, fmt.Errorf("%s is a binary file (%s) and can't be shown as text", shown, sizeText(int64(len(data))))
	}
	return decodeText(data), data, nil
}

// writeFile writes data to path, creating parent directories, and keeps an
// existing file's permissions.
func writeFile(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func sizeText(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

// countLines counts the lines of a text, a last line without a newline
// included.
func countLines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}
