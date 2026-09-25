// Package logging configures the process-wide slog logger and a rotating log
// file writer (no external dependencies).
//
// Files are named <dir>/backend-YYYY-MM-DD.log. A new file starts at local
// midnight; a file that exceeds MaxBytes is renamed to backend-YYYY-MM-DD.N.log
// and a fresh one is opened. Files older than MaxDays are deleted.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Output   string // "file" (default), "stdout" or "both"
	Dir      string // log directory for file output
	Format   string // "json" (default) or "text"
	Level    slog.Level
	MaxBytes int64 // rotate when a file would exceed this size
	MaxDays  int   // delete log files older than this; 0 keeps everything
}

// Setup installs the default slog logger and returns the writer everything
// else (e.g. panic recovery) should log to, plus a closer for shutdown.
func Setup(cfg Config) (io.Writer, func() error, error) {
	var (
		w      io.Writer
		closer = func() error { return nil }
	)
	switch cfg.Output {
	case "stdout":
		w = os.Stdout
	case "file", "both", "":
		rw, err := NewRotatingWriter(cfg.Dir, "backend", cfg.MaxBytes, cfg.MaxDays, time.Now)
		if err != nil {
			return nil, nil, err
		}
		w, closer = rw, rw.Close
		if cfg.Output == "both" {
			w = io.MultiWriter(rw, os.Stdout)
		}
	default:
		return nil, nil, fmt.Errorf("logging: LOG_OUTPUT must be file, stdout or both (got %q)", cfg.Output)
	}

	opts := &slog.HandlerOptions{Level: cfg.Level}
	var h slog.Handler = slog.NewJSONHandler(w, opts)
	if cfg.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))
	return w, closer, nil
}

// RotatingWriter is an io.Writer that rotates daily and by size. Safe for concurrent use.
type RotatingWriter struct {
	dir, prefix string
	maxBytes    int64
	maxDays     int
	now         func() time.Time

	mu   sync.Mutex
	f    *os.File
	day  string
	size int64
}

func NewRotatingWriter(dir, prefix string, maxBytes int64, maxDays int, now func() time.Time) (*RotatingWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logging: create %s: %w", dir, err)
	}
	w := &RotatingWriter{dir: dir, prefix: prefix, maxBytes: maxBytes, maxDays: maxDays, now: now}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) path(day string) string {
	return filepath.Join(w.dir, fmt.Sprintf("%s-%s.log", w.prefix, day))
}

// Path returns the file currently being written.
func (w *RotatingWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path(w.day)
}

func (w *RotatingWriter) open() error {
	w.day = w.now().Format("2006-01-02")
	f, err := os.OpenFile(w.path(w.day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: open: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	w.prune()
	return nil
}

func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if day := w.now().Format("2006-01-02"); day != w.day {
		w.f.Close()
		if err := w.open(); err != nil {
			return 0, err
		}
	} else if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rollBySize(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rollBySize renames the full file to the next free .N suffix and reopens.
func (w *RotatingWriter) rollBySize() error {
	w.f.Close()
	cur := w.path(w.day)
	for n := 1; ; n++ {
		dst := strings.TrimSuffix(cur, ".log") + fmt.Sprintf(".%d.log", n)
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			if err := os.Rename(cur, dst); err != nil {
				return fmt.Errorf("logging: rotate: %w", err)
			}
			break
		}
	}
	return w.open()
}

// prune deletes this writer's log files older than maxDays.
func (w *RotatingWriter) prune() {
	if w.maxDays <= 0 {
		return
	}
	cutoff := w.now().AddDate(0, 0, -w.maxDays)
	matches, _ := filepath.Glob(filepath.Join(w.dir, w.prefix+"-*.log"))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.ModTime().Before(cutoff) && m != w.path(w.day) {
			os.Remove(m)
		}
	}
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
