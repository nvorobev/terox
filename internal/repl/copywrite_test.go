package repl

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicWriteFileSuccess: успешный run пишет содержимое и не оставляет
// временных файлов в каталоге назначения.
func TestAtomicWriteFileSuccess(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.csv")
	n, err := atomicWriteFile(dst, func(w io.Writer) (int64, error) {
		_, err := io.WriteString(w, "a,b\n1,2\n")
		return 1, err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "a,b\n1,2\n" {
		t.Errorf("dst content = %q", string(got))
	}
	assertNoTempLeftovers(t, dir, "out.csv")
}

// TestAtomicWriteFileErrorLeavesDestinationUntouched: если run возвращает ошибку
// (сервер отверг COPY), существующий файл назначения остаётся прежним, а
// временный файл не остаётся на диске (R-NEW-4).
func TestAtomicWriteFileErrorLeavesDestinationUntouched(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.csv")
	const original = "OLD-RESULT\n"
	if err := os.WriteFile(dst, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := atomicWriteFile(dst, func(w io.Writer) (int64, error) {
		// Пишем мусор, затем падаем — это типичный сценарий, когда сервер начал
		// поток COPY, но затем вернул ошибку.
		_, _ = io.WriteString(w, "partial garbage that must not survive")
		return 0, fmt.Errorf("server rejected COPY")
	})
	if err == nil {
		t.Fatal("expected error from run")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != original {
		t.Errorf("destination was modified on failure: got %q, want %q", string(got), original)
	}
	assertNoTempLeftovers(t, dir, "out.csv")
}

// TestAtomicWriteFileNewFileError: если файла назначения не было и run упал, файл
// не создаётся (никакого пустого огрызка).
func TestAtomicWriteFileNewFileError(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "fresh.csv")
	_, err := atomicWriteFile(dst, func(w io.Writer) (int64, error) {
		return 0, fmt.Errorf("boom")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("destination should not exist after failed write, stat err = %v", statErr)
	}
	assertNoTempLeftovers(t, dir, "fresh.csv")
}

func assertNoTempLeftovers(t *testing.T, dir, dstName string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != dstName {
			t.Errorf("unexpected leftover file in dir: %q", e.Name())
		}
	}
}
