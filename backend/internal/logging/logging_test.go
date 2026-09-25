package logging

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func files(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	for i := range m {
		m[i] = filepath.Base(m[i])
	}
	sort.Strings(m)
	return m
}

func TestRotatesBySize(t *testing.T) {
	dir := t.TempDir()
	now := func() time.Time { return time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local) }
	w, err := NewRotatingWriter(dir, "backend", 100, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := []byte(strings.Repeat("x", 39) + "\n") // 40 bytes
	for i := 0; i < 7; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	got := files(t, dir)
	want := []string{"backend-2026-09-26.1.log", "backend-2026-09-26.2.log", "backend-2026-09-26.3.log", "backend-2026-09-26.log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files=%v want %v", got, want)
	}
	for _, f := range got {
		if st, _ := os.Stat(filepath.Join(dir, f)); st.Size() > 100 {
			t.Fatalf("%s is %d bytes, over the limit", f, st.Size())
		}
	}
}

func TestRotatesDailyAndPrunes(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "backend-2026-09-01.log")
	os.WriteFile(old, []byte("old\n"), 0o644)
	os.Chtimes(old, time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local))
	other := filepath.Join(dir, "unrelated.log") // not ours: never deleted
	os.WriteFile(other, []byte("keep\n"), 0o644)
	os.Chtimes(other, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))

	clock := time.Date(2026, 9, 26, 23, 59, 0, 0, time.Local)
	w, err := NewRotatingWriter(dir, "backend", 0, 7, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Write([]byte("a\n"))
	clock = clock.Add(2 * time.Minute) // past midnight
	w.Write([]byte("b\n"))

	got := strings.Join(files(t, dir), ",")
	if got != "backend-2026-09-26.log,backend-2026-09-27.log,unrelated.log" {
		t.Fatalf("files=%s", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "backend-2026-09-27.log"))
	if string(b) != "b\n" {
		t.Fatalf("new day file content %q", b)
	}
}

func TestWriteAfterClose(t *testing.T) {
	w, err := NewRotatingWriter(t.TempDir(), "backend", 0, 0, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("write after close should fail")
	}
}
