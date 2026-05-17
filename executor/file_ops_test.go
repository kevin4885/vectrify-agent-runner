package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestOps creates a FileOps rooted at a fresh temp directory.
// The caller is responsible for calling cleanup() when done.
func newTestOps(t *testing.T) (*FileOps, string, func()) {
	t.Helper()
	dir := t.TempDir()
	ops := NewFileOps(dir)
	cleanup := func() { os.RemoveAll(dir) }
	return ops, dir, cleanup
}

// writeRaw writes raw bytes directly to a file inside the temp workspace.
func writeRaw(t *testing.T, root, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.WriteFile(p, content, 0644); err != nil {
		t.Fatalf("writeRaw: %v", err)
	}
	return p
}

// readRaw reads raw bytes from a file for assertion.
func readRaw(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readRaw: %v", err)
	}
	return data
}

// ─────────────────────────────────────────────────────────────────────────────
// guardPath
// ─────────────────────────────────────────────────────────────────────────────

func TestGuardPath_Inside(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	got, err := ops.guardPath(filepath.Join(root, "file.txt"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty clean path")
	}
}

func TestGuardPath_Outside(t *testing.T) {
	ops, _, cleanup := newTestOps(t)
	defer cleanup()

	_, err := ops.guardPath(os.TempDir()) // parent of workspace root
	if err == nil {
		t.Fatal("expected error for path outside workspace, got nil")
	}
}

func TestGuardPath_Empty(t *testing.T) {
	ops, _, cleanup := newTestOps(t)
	defer cleanup()

	_, err := ops.guardPath("")
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ReadFile — view
// ─────────────────────────────────────────────────────────────────────────────

func TestReadFile_LFFile(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	writeRaw(t, root, "lf.txt", []byte("line1\nline2\nline3\n"))

	out, err := ops.ReadFile(filepath.Join(root, "lf.txt"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "1: line1") {
		t.Errorf("expected line numbers, got:\n%s", out)
	}
	if strings.Contains(out, "\r") {
		t.Errorf("output must not contain \\r, got:\n%q", out)
	}
}

func TestReadFile_CRLFFile_NoCarriageReturn(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	// CRLF file — simulates a Windows-native file
	writeRaw(t, root, "crlf.txt", []byte("line1\r\nline2\r\nline3\r\n"))

	out, err := ops.ReadFile(filepath.Join(root, "crlf.txt"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The LLM must never see a trailing \r on a line
	if strings.Contains(out, "\r") {
		t.Errorf("ReadFile must strip \\r from CRLF files, got:\n%q", out)
	}
	if !strings.Contains(out, "1: line1") {
		t.Errorf("expected numbered lines, got:\n%s", out)
	}
}

func TestReadFile_ViewRange(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	writeRaw(t, root, "range.txt", []byte("a\nb\nc\nd\ne\n"))

	out, err := ops.ReadFile(filepath.Join(root, "range.txt"), []int{2, 4})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "1: a") {
		t.Errorf("line 1 should not appear in range [2,4], got:\n%s", out)
	}
	if !strings.Contains(out, "2: b") {
		t.Errorf("line 2 should appear, got:\n%s", out)
	}
}

func TestReadFile_Directory(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	writeRaw(t, root, "sample.go", []byte("package main\n"))

	out, err := ops.ReadFile(root, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "sample.go") {
		t.Errorf("expected directory listing to contain sample.go, got:\n%s", out)
	}
}

func TestReadFile_NotFound(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	_, err := ops.ReadFile(filepath.Join(root, "missing.txt"), nil)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// WriteFile — create
// ─────────────────────────────────────────────────────────────────────────────

func TestWriteFile_LFContent(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := filepath.Join(root, "out.txt")
	if err := ops.WriteFile(p, "hello\nworld\n"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := readRaw(t, p)
	if strings.Contains(string(raw), "\r\n") {
		t.Errorf("WriteFile must not introduce CRLF, got: %q", raw)
	}
}

func TestWriteFile_CRLFContentNormalized(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := filepath.Join(root, "out_crlf.txt")
	// Supply CRLF content — should land on disk as LF
	if err := ops.WriteFile(p, "hello\r\nworld\r\n"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := readRaw(t, p)
	if strings.Contains(string(raw), "\r\n") {
		t.Errorf("WriteFile must normalize CRLF to LF, got: %q", raw)
	}
	if !strings.Contains(string(raw), "hello\nworld") {
		t.Errorf("content mismatch after normalization: %q", raw)
	}
}

func TestWriteFile_CreatesParentDirs(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := filepath.Join(root, "sub", "dir", "file.txt")
	if err := ops.WriteFile(p, "content\n"); err != nil {
		t.Fatalf("expected WriteFile to create parent dirs: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// StrReplace — str_replace
// ─────────────────────────────────────────────────────────────────────────────

func TestStrReplace_LFFile_LFOldStr(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "lf.py", []byte("def foo():\n    pass\n"))

	msg, err := ops.StrReplace(p, "def foo():\n    pass", "def foo():\n    return 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg, "Successfully") {
		t.Errorf("unexpected message: %s", msg)
	}
	raw := string(readRaw(t, p))
	if !strings.Contains(raw, "return 1") {
		t.Errorf("replacement not applied: %q", raw)
	}
}

func TestStrReplace_CRLFFile_LFOldStr(t *testing.T) {
	// Core scenario: file has CRLF on disk, LLM sends plain LF in oldStr
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "crlf.py", []byte("def foo():\r\n    pass\r\n"))

	msg, err := ops.StrReplace(p, "def foo():\n    pass", "def foo():\n    return 1")
	if err != nil {
		t.Fatalf("CRLF file with LF oldStr must match (got error): %v", err)
	}
	if !strings.Contains(msg, "Successfully") {
		t.Errorf("unexpected message: %s", msg)
	}
	raw := string(readRaw(t, p))
	if !strings.Contains(raw, "return 1") {
		t.Errorf("replacement not applied to CRLF file: %q", raw)
	}
	// After replacement file should be LF
	if strings.Contains(raw, "\r\n") {
		t.Errorf("file should be written as LF after StrReplace: %q", raw)
	}
}

func TestStrReplace_CRLFFile_CRLFOldStr(t *testing.T) {
	// Both file and oldStr are CRLF — should still work
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "both_crlf.py", []byte("x = 1\r\ny = 2\r\n"))

	_, err := ops.StrReplace(p, "x = 1\r\ny = 2", "x = 10\ny = 20")
	if err != nil {
		t.Fatalf("CRLF file with CRLF oldStr must match: %v", err)
	}
	raw := string(readRaw(t, p))
	if !strings.Contains(raw, "x = 10") {
		t.Errorf("replacement not applied: %q", raw)
	}
}

func TestStrReplace_NotFound(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "f.txt", []byte("hello world\n"))

	_, err := ops.StrReplace(p, "goodbye world", "hi")
	if err == nil {
		t.Fatal("expected 'not found' error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestStrReplace_MultipleOccurrences(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "dup.txt", []byte("foo\nfoo\n"))

	_, err := ops.StrReplace(p, "foo", "bar")
	if err == nil {
		t.Fatal("expected 'multiple occurrences' error, got nil")
	}
	if !strings.Contains(err.Error(), "occurrences") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestStrReplace_WritesLFAfterReplace(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	// Mixed line endings edge case
	p := writeRaw(t, root, "mixed.txt", []byte("a\r\nb\nc\r\n"))

	_, err := ops.StrReplace(p, "a\nb", "x\ny")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := readRaw(t, p)
	if strings.Contains(string(raw), "\r\n") {
		t.Errorf("file must be written with LF after StrReplace: %q", raw)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Insert
// ─────────────────────────────────────────────────────────────────────────────

func TestInsert_LFFile(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "ins.txt", []byte("line1\nline2\nline3\n"))

	msg, err := ops.Insert(p, 1, "inserted")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg, "Successfully") {
		t.Errorf("unexpected message: %s", msg)
	}
	raw := string(readRaw(t, p))
	if !strings.Contains(raw, "inserted") {
		t.Errorf("inserted text not found: %q", raw)
	}
}

func TestInsert_CRLFFile_WritesLF(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "crlf_ins.txt", []byte("line1\r\nline2\r\nline3\r\n"))

	_, err := ops.Insert(p, 1, "newline")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := readRaw(t, p)
	if strings.Contains(string(raw), "\r\n") {
		t.Errorf("Insert must write LF after operating on CRLF file: %q", raw)
	}
	if !strings.Contains(string(raw), "newline") {
		t.Errorf("inserted text not found: %q", raw)
	}
}

func TestInsert_AtEnd(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "end.txt", []byte("a\nb\n"))

	_, err := ops.Insert(p, 2, "c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := string(readRaw(t, p))
	if !strings.Contains(raw, "c") {
		t.Errorf("expected 'c' at end: %q", raw)
	}
}

func TestInsert_InvalidLine(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "short.txt", []byte("a\nb\n"))

	_, err := ops.Insert(p, 999, "x")
	if err == nil {
		t.Fatal("expected error for out-of-range line number, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteFile
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteFile_Exists(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "del.txt", []byte("bye\n"))

	if err := ops.DeleteFile(p); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file should have been deleted")
	}
}

func TestDeleteFile_Missing(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	err := ops.DeleteFile(filepath.Join(root, "ghost.txt"))
	if err == nil {
		t.Fatal("expected error deleting non-existent file, got nil")
	}
}
