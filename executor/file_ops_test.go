package executor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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
	// The file's CRLF style is preserved byte-for-byte.
	if want := "def foo():\r\n    return 1\r\n"; raw != want {
		t.Errorf("CRLF style must be preserved: got %q, want %q", raw, want)
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

func TestStrReplace_MixedEndings_MajorityWins(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"majority CRLF", "a\r\nb\nc\r\n", "x\r\ny\r\nc\r\n"},
		{"majority LF", "a\nb\r\nc\n", "x\ny\nc\n"},
		{"tie is LF", "a\r\nb\n", "x\ny\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()

			p := writeRaw(t, root, "mixed.txt", []byte(tc.in))
			if _, err := ops.StrReplace(p, "a\nb", "x\ny"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := string(readRaw(t, p)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
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

func TestInsert_CRLFFile_PreservesCRLF(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "crlf_ins.txt", []byte("line1\r\nline2\r\nline3\r\n"))

	_, err := ops.Insert(p, 1, "newline")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := string(readRaw(t, p)), "line1\r\nnewline\r\nline2\r\nline3\r\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
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
// Line-ending preservation (C1)
// ─────────────────────────────────────────────────────────────────────────────

func TestDetectLineEnding(t *testing.T) {
	cases := []struct {
		name string
		in   string
		crlf bool
	}{
		{"empty", "", false},
		{"no newline", "abc", false},
		{"lf", "a\nb\n", false},
		{"crlf", "a\r\nb\r\n", true},
		{"majority crlf", "a\r\nb\nc\r\n", true},
		{"majority lf", "a\nb\r\nc\n", false},
		{"tie", "a\r\nb\n", false},
		{"lone cr ignored", "a\rb\r\n", true},
		{"leading lf", "\nabc", false},
	}
	for _, tc := range cases {
		if got := detectLineEnding([]byte(tc.in)); got != tc.crlf {
			t.Errorf("%s: detectLineEnding(%q) = %v, want %v", tc.name, tc.in, got, tc.crlf)
		}
	}
}

func TestStrReplace_LFFile_NoCarriageReturn(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "lf.txt", []byte("a\nb\nc\n"))
	// CRLF in the arguments must not leak into an LF file.
	if _, err := ops.StrReplace(p, "a\r\nb", "x\r\ny\r\nz"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(readRaw(t, p))
	if want := "x\ny\nz\nc\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("LF file must contain no \\r: %q", got)
	}
}

func TestStrReplace_CRLFFile_MultiLineNewStr(t *testing.T) {
	cases := []struct {
		name, newStr string
	}{
		{"LF new_str", "x\ny\nz"},
		{"CRLF new_str", "x\r\ny\r\nz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()

			p := writeRaw(t, root, "c.txt", []byte("a\r\nb\r\nc\r\n"))
			if _, err := ops.StrReplace(p, "b", tc.newStr); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := string(readRaw(t, p))
			if want := "a\r\nx\r\ny\r\nz\r\nc\r\n"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
			if strings.Contains(got, "\r\r") {
				t.Errorf("must not contain \\r\\r: %q", got)
			}
			if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
				t.Errorf("bare LF found in CRLF file: %q", got)
			}
		})
	}
}

func TestStrReplace_FinalNewlinePreserved(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"crlf without trailing newline", "a\r\nb", "a\r\nX"},
		{"crlf with trailing newline", "a\r\nb\r\n", "a\r\nX\r\n"},
		{"lf without trailing newline", "a\nb", "a\nX"},
		{"lf with trailing newline", "a\nb\n", "a\nX\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()

			p := writeRaw(t, root, "f.txt", []byte(tc.in))
			if _, err := ops.StrReplace(p, "b", "X"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := string(readRaw(t, p)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStrReplace_ReturnString(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "f.txt", []byte("a\r\n"))
	msg, err := ops.StrReplace(p, "a", "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Successfully replaced text at exactly one location."; msg != want {
		t.Errorf("got %q, want %q", msg, want)
	}
}

func TestInsert_Positions(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		line   int
		newStr string
		want   string
	}{
		{"crlf after line 1", "a\r\nb\r\nc\r\n", 1, "X", "a\r\nX\r\nb\r\nc\r\n"},
		{"crlf middle", "a\r\nb\r\nc\r\nd\r\n", 2, "X", "a\r\nb\r\nX\r\nc\r\nd\r\n"},
		{"crlf at last line", "a\r\nb\r\nc\r\n", 3, "X", "a\r\nb\r\nc\r\nX\r\n"},
		{"crlf at last line no final newline", "a\r\nb\r\nc", 3, "X", "a\r\nb\r\nc\r\nX"},
		{"crlf at line 0", "a\r\nb\r\n", 0, "X", "X\r\na\r\nb\r\n"},
		{"crlf multi-line LF insert", "a\r\nb\r\n", 1, "X\nY", "a\r\nX\r\nY\r\nb\r\n"},
		{"crlf multi-line CRLF insert", "a\r\nb\r\n", 1, "X\r\nY", "a\r\nX\r\nY\r\nb\r\n"},
		{"crlf insert with trailing newline", "a\r\nb\r\n", 1, "X\n", "a\r\nX\r\nb\r\n"},
		{"lf after line 1", "a\nb\nc\n", 1, "X", "a\nX\nb\nc\n"},
		{"lf at last line", "a\nb\nc\n", 3, "X", "a\nb\nc\nX\n"},
		{"lf at last line no final newline", "a\nb\nc", 3, "X", "a\nb\nc\nX"},
		{"lf insert with CRLF text", "a\nb\n", 1, "X\r\nY", "a\nX\nY\nb\n"},
		{"mixed majority crlf", "a\r\nb\nc\r\n", 1, "X", "a\r\nX\r\nb\r\nc\r\n"},
		{"mixed majority lf", "a\nb\r\nc\n", 1, "X", "a\nX\nb\nc\n"},
		{"mixed tie is lf", "a\r\nb\n", 1, "X", "a\nX\nb\n"},
		{"empty file is lf", "", 0, "X", "X\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()

			p := writeRaw(t, root, "f.txt", []byte(tc.in))
			msg, err := ops.Insert(p, tc.line, tc.newStr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := fmt.Sprintf("Successfully inserted text after line %d.", tc.line); msg != want {
				t.Errorf("message: got %q, want %q", msg, want)
			}
			if got := string(readRaw(t, p)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteFile_OverExistingFile_UsesExistingStyle(t *testing.T) {
	cases := []struct {
		name, existing, content, want string
	}{
		{"CRLF file, LF content", "old\r\nfile\r\n", "new\nbody\n", "new\r\nbody\r\n"},
		{"CRLF file, CRLF content", "old\r\nfile\r\n", "new\r\nbody\r\n", "new\r\nbody\r\n"},
		{"LF file, CRLF content", "old\nfile\n", "new\r\nbody\r\n", "new\nbody\n"},
		{"LF file, LF content", "old\nfile\n", "new\nbody\n", "new\nbody\n"},
		{"empty file, CRLF content", "", "new\r\nbody\r\n", "new\nbody\n"},
		{"empty file, LF content", "", "new\nbody", "new\nbody"},
		{"CRLF file, no final newline", "old\r\nfile", "new\nbody", "new\r\nbody"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()

			p := writeRaw(t, root, "f.txt", []byte(tc.existing))
			if err := ops.WriteFile(p, tc.content); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := string(readRaw(t, p)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteFile_NewFile_AlwaysLF(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"LF content", "a\nb\n", "a\nb\n"},
		{"CRLF content", "a\r\nb\r\n", "a\nb\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()

			p := filepath.Join(root, "new.txt")
			if err := ops.WriteFile(p, tc.content); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := string(readRaw(t, p)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEditFailures_LeaveFileUntouched(t *testing.T) {
	const orig = "foo\r\nfoo\r\nbar\r\n"
	run := func(t *testing.T, name string, op func(ops *FileOps, p string) (string, error), wantErr string) {
		t.Run(name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()

			p := writeRaw(t, root, "f.txt", []byte(orig))
			_, err := op(ops, p)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != wantErr {
				t.Errorf("error: got %q, want %q", err.Error(), wantErr)
			}
			if got := string(readRaw(t, p)); got != orig {
				t.Errorf("file modified on error path: %q", got)
			}
		})
	}
	run(t, "str_replace not found", func(ops *FileOps, p string) (string, error) {
		return ops.StrReplace(p, "missing", "x")
	}, "the specified text was not found in the file")
	run(t, "str_replace multiple", func(ops *FileOps, p string) (string, error) {
		return ops.StrReplace(p, "foo", "x")
	}, "found 2 occurrences of the specified text — must be unique")
	// "foo\r\nfoo\r\nbar\r\n" splits into 4 elements (trailing empty one).
	run(t, "insert negative line", func(ops *FileOps, p string) (string, error) {
		return ops.Insert(p, -1, "x")
	}, "invalid line number -1 (file has 4 lines)")
	run(t, "insert past end", func(ops *FileOps, p string) (string, error) {
		return ops.Insert(p, 99, "x")
	}, "invalid line number 99 (file has 4 lines)")
}

func TestEdit_NonexistentPath(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := filepath.Join(root, "nope.txt")
	if _, err := ops.StrReplace(p, "a", "b"); err == nil || err.Error() != "file not found: "+p {
		t.Errorf("StrReplace: got %v", err)
	}
	if _, err := ops.Insert(p, 0, "b"); err == nil || err.Error() != "file not found: "+p {
		t.Errorf("Insert: got %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("edit of a nonexistent path must not create the file")
	}
}

func TestEdit_OutsideWorkspaceRejected(t *testing.T) {
	ops, _, cleanup := newTestOps(t)
	defer cleanup()

	outside := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(outside, []byte("a\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.StrReplace(outside, "a", "b"); err == nil {
		t.Error("StrReplace outside workspace must be rejected")
	}
	if _, err := ops.Insert(outside, 0, "b"); err == nil {
		t.Error("Insert outside workspace must be rejected")
	}
	if err := ops.WriteFile(outside, "b"); err == nil {
		t.Error("WriteFile outside workspace must be rejected")
	}
	if got := string(readRaw(t, outside)); got != "a\r\n" {
		t.Errorf("outside file modified: %q", got)
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

// ─────────────────────────────────────────────────────────────────────────────
// Concurrency — per-path locking (regression for the session-4513 file
// corruption bug: two tool calls targeting the SAME path in the same LLM
// turn used to race — read-modify-write with no lock meant the second
// writer clobbered the first with a stale snapshot). These tests fire
// many concurrent operations at ONE path and assert every one of them is
// observable afterward, which is only possible if they were serialized.
// ─────────────────────────────────────────────────────────────────────────────

// TestStrReplace_ConcurrentAppends_NoLostWrites appends N distinct,
// non-overlapping anchor-replace edits to the same file from N goroutines
// simultaneously (the exact "append to activity-log.md anchor" pattern
// growth-strategist uses). Before the per-path lock this reliably lost
// writes under `go test -race`; every edit must now survive.
func TestStrReplace_ConcurrentAppends_NoLostWrites(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	const anchor = "<!-- anchor -->\n"
	p := writeRaw(t, root, "log.md", []byte(anchor))

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			line := fmt.Sprintf("- entry-%02d\n", i)
			_, err := ops.StrReplace(p, anchor, anchor+line)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}

	data := readRaw(t, p)
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("- entry-%02d", i)
		if !strings.Contains(string(data), want) {
			t.Errorf("lost write: %q missing from final file — got:\n%s", want, data)
		}
	}
}

// TestFileOps_ConcurrentMixedOps_SamePath_RaceDetectorClean hammers every
// read-modify-write entry point (WriteFile, StrReplace via unique markers,
// Insert, ReadFile) concurrently against the same path. It doesn't assert
// a specific final state (WriteFile/StrReplace on the same path is
// inherently a last-write-wins ordering choice) — its job is to make sure
// `go test -race` finds no data race in FileOps itself now that every
// operation holds the per-path lock for its full duration.
func TestFileOps_ConcurrentMixedOps_SamePath_RaceDetectorClean(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "mixed.md", []byte("line1\nline2\nline3\n"))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = ops.ReadFile(p, nil)
			_ = ops.WriteFile(p, fmt.Sprintf("line1\nline2\nline3\nwrite-%d\n", i))
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = ops.Insert(p, 1, fmt.Sprintf("insert-%d", i))
		}(i)
	}
	wg.Wait()

	// File must still be readable and non-empty — a torn/interleaved write
	// would typically show up as corrupted content or a read error.
	data := readRaw(t, p)
	if len(data) == 0 {
		t.Fatal("file ended up empty after concurrent mixed ops")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// File integrity (C1'): atomic writes, BOM, binary refusal, validation
// ─────────────────────────────────────────────────────────────────────────────

// listNames returns the names of all entries in dir.
func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestAtomicWriteFile_SuccessReplacesContentLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("old content that is longer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(p, []byte("new")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(readRaw(t, p)); got != "new" {
		t.Errorf("content: got %q, want %q", got, "new")
	}
	if names := listNames(t, dir); len(names) != 1 || names[0] != "f.txt" {
		t.Errorf("temp files left behind: %v", names)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("mode bits not preserved: got %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestAtomicWriteFile_NewFileMode0644(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new.txt")
	if err := atomicWriteFile(p, []byte("x")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0644 {
			t.Errorf("new file mode: got %v, want 0644", info.Mode().Perm())
		}
	}
	if got := string(readRaw(t, p)); got != "x" {
		t.Errorf("content: got %q", got)
	}
}

func TestAtomicWriteFile_RenameFailure_OriginalUntouchedTempRemoved(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	orig := []byte("original\r\ncontent\r\n")
	if err := os.WriteFile(p, orig, 0644); err != nil {
		t.Fatal(err)
	}
	saved := renameFile
	renameFile = func(oldpath, newpath string) error { return errors.New("injected rename failure") }
	defer func() { renameFile = saved }()

	if err := atomicWriteFile(p, []byte("replacement")); err == nil {
		t.Fatal("expected error from injected rename failure, got nil")
	}
	if got := readRaw(t, p); !bytes.Equal(got, orig) {
		t.Errorf("original modified: got %q, want %q", got, orig)
	}
	if names := listNames(t, dir); len(names) != 1 || names[0] != "f.txt" {
		t.Errorf("temp file not removed: %v", names)
	}
}

func TestEdits_RenameFailure_LeaveOriginalIntact(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	orig := []byte("a\r\nb\r\n")
	p := writeRaw(t, root, "f.txt", orig)
	saved := renameFile
	renameFile = func(oldpath, newpath string) error { return errors.New("injected rename failure") }
	defer func() { renameFile = saved }()

	if _, err := ops.StrReplace(p, "a", "x"); err == nil {
		t.Error("StrReplace: expected error")
	}
	if _, err := ops.Insert(p, 1, "x"); err == nil {
		t.Error("Insert: expected error")
	}
	if err := ops.WriteFile(p, "zzz"); err == nil {
		t.Error("WriteFile: expected error")
	}
	if got := readRaw(t, p); !bytes.Equal(got, orig) {
		t.Errorf("original modified: %q", got)
	}
	if names := listNames(t, root); len(names) != 1 {
		t.Errorf("temp file left behind: %v", names)
	}
}

func TestStrReplace_ThroughSymlink_LinkSurvives(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	target := writeRaw(t, root, "target.txt", []byte("hello\r\nworld\r\n"))
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if _, err := ops.StrReplace(link, "hello", "bye"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("link was replaced by a regular file")
	}
	if got, want := string(readRaw(t, target)), "bye\r\nworld\r\n"; got != want {
		t.Errorf("target content: got %q, want %q", got, want)
	}
	if _, err := ops.Insert(link, 0, "top"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Error("link was replaced by a regular file after insert")
	}
	if got, want := string(readRaw(t, target)), "top\r\nbye\r\nworld\r\n"; got != want {
		t.Errorf("target content after insert: got %q, want %q", got, want)
	}
}

func TestBOM_Preserved(t *testing.T) {
	bom := "\xEF\xBB\xBF"
	t.Run("str_replace", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := writeRaw(t, root, "f.txt", []byte(bom+"a\r\nb\r\n"))
		if _, err := ops.StrReplace(p, "a", "X"); err != nil {
			t.Fatal(err)
		}
		if got, want := string(readRaw(t, p)), bom+"X\r\nb\r\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("str_replace can match first line text", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := writeRaw(t, root, "f.txt", []byte(bom+"a\nb\n"))
		if _, err := ops.StrReplace(p, "a\nb", "Q"); err != nil {
			t.Fatal(err)
		}
		if got, want := string(readRaw(t, p)), bom+"Q\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("insert after line 0", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := writeRaw(t, root, "f.txt", []byte(bom+"a\r\nb\r\n"))
		if _, err := ops.Insert(p, 0, "X"); err != nil {
			t.Fatal(err)
		}
		if got, want := string(readRaw(t, p)), bom+"X\r\na\r\nb\r\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("create over BOM file keeps BOM", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := writeRaw(t, root, "f.txt", []byte(bom+"a\r\nb\r\n"))
		if err := ops.WriteFile(p, "new\nbody\n"); err != nil {
			t.Fatal(err)
		}
		if got, want := string(readRaw(t, p)), bom+"new\r\nbody\r\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("create with own U+FEFF not doubled", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := writeRaw(t, root, "f.txt", []byte(bom+"a\r\n"))
		if err := ops.WriteFile(p, "\uFEFFnew\n"); err != nil {
			t.Fatal(err)
		}
		if got, want := string(readRaw(t, p)), bom+"new\r\n"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("new file gets no BOM", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := filepath.Join(root, "new.txt")
		if err := ops.WriteFile(p, "hi\n"); err != nil {
			t.Fatal(err)
		}
		if got := string(readRaw(t, p)); got != "hi\n" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("file without BOM stays without BOM", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := writeRaw(t, root, "f.txt", []byte("a\nb\n"))
		if _, err := ops.StrReplace(p, "a", "X"); err != nil {
			t.Fatal(err)
		}
		if got := string(readRaw(t, p)); got != "X\nb\n" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("view hides BOM", func(t *testing.T) {
		ops, root, cleanup := newTestOps(t)
		defer cleanup()
		p := writeRaw(t, root, "f.txt", []byte(bom+"a\r\nb\r\n"))
		out, err := ops.ReadFile(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "\uFEFF") {
			t.Errorf("view contains U+FEFF: %q", out)
		}
		if want := "1: a\n2: b\n"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
}

func TestEdit_RefusesBinaryAndUTF16(t *testing.T) {
	const wantErr = "refusing to edit a binary or UTF-16 file; use runner_shell"
	late := append(bytes.Repeat([]byte("a"), binarySniffLen+10), 0)
	cases := []struct {
		name    string
		content []byte
		refused bool
	}{
		{"NUL in first bytes", []byte("abc\x00def abc"), true},
		{"UTF-16LE BOM", []byte{0xFF, 0xFE, 'a', 0, ' ', 0, 'a', 0}, true},
		{"UTF-16BE BOM", []byte{0xFE, 0xFF, 0, 'a', 0, ' ', 0, 'a'}, true},
		{"NUL only after sniff window", late, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, root, cleanup := newTestOps(t)
			defer cleanup()
			p := writeRaw(t, root, "b.bin", tc.content)

			_, err1 := ops.StrReplace(p, "a", "z")
			_, err2 := ops.Insert(p, 0, "z")
			if tc.refused {
				for i, err := range []error{err1, err2} {
					if err == nil || err.Error() != wantErr {
						t.Errorf("op %d: got %v, want %q", i, err, wantErr)
					}
				}
				if got := readRaw(t, p); !bytes.Equal(got, tc.content) {
					t.Errorf("bytes changed: %q", got)
				}
				// create over it is still allowed
				if err := ops.WriteFile(p, "text\n"); err != nil {
					t.Errorf("create over binary must be allowed: %v", err)
				}
				if got := string(readRaw(t, p)); got != "text\n" {
					t.Errorf("create over binary: got %q", got)
				}
			} else if err1 != nil && err1.Error() == wantErr {
				t.Error("NUL beyond the sniff window must not trigger refusal")
			}
		})
	}
}

func TestStrReplace_EmptyOldStrRejected(t *testing.T) {
	for _, content := range []string{"", "some\ncontent\n"} {
		ops, root, cleanup := newTestOps(t)
		p := writeRaw(t, root, "f.txt", []byte(content))
		_, err := ops.StrReplace(p, "", "x")
		if err == nil || err.Error() != "old_str must not be empty" {
			t.Errorf("content %q: got %v", content, err)
		}
		if got := string(readRaw(t, p)); got != content {
			t.Errorf("file modified: %q", got)
		}
		cleanup()
	}
}

func TestOps_RelativePathRejected(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()
	writeRaw(t, root, "foo.txt", []byte("a\n"))

	for _, rel := range []string{"foo.txt", ".\\foo.txt", "./foo.txt", "sub/foo.txt"} {
		want := "path must be absolute: " + rel
		check := func(op string, err error) {
			if err == nil || err.Error() != want {
				t.Errorf("%s(%q): got %v, want %q", op, rel, err, want)
			}
		}
		_, err := ops.ReadFile(rel, nil)
		check("view", err)
		check("create", ops.WriteFile(rel, "x"))
		_, err = ops.StrReplace(rel, "a", "b")
		check("str_replace", err)
		_, err = ops.Insert(rel, 0, "b")
		check("insert", err)
		check("delete", ops.DeleteFile(rel))
	}
	if got := string(readRaw(t, filepath.Join(root, "foo.txt"))); got != "a\n" {
		t.Errorf("file modified: %q", got)
	}
	// Absolute path inside the root still works.
	if _, err := ops.ReadFile(filepath.Join(root, "foo.txt"), nil); err != nil {
		t.Errorf("absolute path inside root rejected: %v", err)
	}
}

func TestReadFile_ViewRangeValidation(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()
	p := writeRaw(t, root, "r.txt", []byte("a\nb\nc\nd\n"))

	_, err := ops.ReadFile(p, []int{30, 10})
	if want := "invalid view_range: start 30 is after end 10"; err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
	_, err = ops.ReadFile(p, []int{3, 2})
	if want := "invalid view_range: start 3 is after end 2"; err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
	out, err := ops.ReadFile(p, []int{0, 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := "1: a\n2: b\n"; out != want {
		t.Errorf("[0,2]: got %q, want %q", out, want)
	}
	// An end of -1 still means "to the end".
	out, err = ops.ReadFile(p, []int{3, -1})
	if err != nil {
		t.Fatal(err)
	}
	if want := "3: c\n4: d\n"; out != want {
		t.Errorf("[3,-1]: got %q, want %q", out, want)
	}
}

func TestReadFile_ClipNeverSplitsRune(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	// Output is "1: " + content + "\n". Put the 3-byte "€" so that it starts at
	// byte 499999 and straddles the 500000-byte limit.
	content := strings.Repeat("a", 499_999-len("1: ")) + "€€€€"
	p := writeRaw(t, root, "big.txt", []byte(content))
	out, err := ops.ReadFile(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out) {
		t.Error("clipped output is not valid UTF-8")
	}
	if !strings.HasSuffix(out, "\n<response clipped>") {
		t.Errorf("missing clip marker: %q", out[len(out)-30:])
	}
	if strings.Contains(out, "€") {
		t.Error("the straddling rune should have been dropped, not kept partially")
	}
}

func TestGuardPath_CaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "Case.txt", []byte("a\r\n"))
	upper := strings.ToUpper(p)
	if upper == p {
		t.Skip("path has no letters to re-case")
	}
	if _, err := ops.guardPath(upper); err != nil {
		t.Errorf("upper-cased spelling rejected: %v", err)
	}
	if _, err := ops.guardPath(strings.ToLower(p)); err != nil {
		t.Errorf("lower-cased spelling rejected: %v", err)
	}
	if _, err := ops.StrReplace(upper, "a", "b"); err != nil {
		t.Errorf("StrReplace via upper-cased path: %v", err)
	}
	// Outside paths are still rejected.
	if _, err := ops.guardPath(filepath.Dir(root)); err == nil {
		t.Error("parent of root must still be rejected")
	}
	if _, err := ops.guardPath(root + "-sibling"); err == nil {
		t.Error("sibling sharing the root prefix must still be rejected")
	}

	// Both spellings must share one lock.
	if lockKey(p) != lockKey(upper) {
		t.Fatalf("lock keys differ: %q vs %q", lockKey(p), lockKey(upper))
	}
	release := ops.lockPath(p)
	acquired := make(chan struct{})
	go func() {
		r := ops.lockPath(upper)
		close(acquired)
		r()
	}()
	select {
	case <-acquired:
		t.Error("second spelling acquired a different lock while the first was held")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Error("second spelling never acquired the lock after release")
	}
}

func TestLockPath_MapReleasedWhenUnused(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	for i := 0; i < 200; i++ {
		release := ops.lockPath(filepath.Join(root, fmt.Sprintf("f%d.txt", i)))
		release()
	}
	ops.mu.Lock()
	n := len(ops.pathLocks)
	ops.mu.Unlock()
	if n != 0 {
		t.Errorf("lock map has %d entries after all locks released, want 0", n)
	}

	// Held lock keeps its entry; contended use still ends at zero.
	p := filepath.Join(root, "shared.txt")
	release := ops.lockPath(p)
	ops.mu.Lock()
	held := len(ops.pathLocks)
	ops.mu.Unlock()
	if held != 1 {
		t.Errorf("held lock: map has %d entries, want 1", held)
	}
	release()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ops.lockPath(p)()
		}()
	}
	wg.Wait()
	ops.mu.Lock()
	n = len(ops.pathLocks)
	ops.mu.Unlock()
	if n != 0 {
		t.Errorf("lock map has %d entries after contended use, want 0", n)
	}
}

func TestCreate_ReportsOverwritten(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := filepath.Join(root, "n.txt")
	over, err := ops.Create(p, "a\n")
	if err != nil || over {
		t.Errorf("new file: overwritten=%v err=%v, want false/nil", over, err)
	}
	over, err = ops.Create(p, "b\n")
	if err != nil || !over {
		t.Errorf("existing file: overwritten=%v err=%v, want true/nil", over, err)
	}
}

func TestWriteFile_OverExisting_DoesNotPreserveFinalNewline(t *testing.T) {
	ops, root, cleanup := newTestOps(t)
	defer cleanup()

	p := writeRaw(t, root, "f.txt", []byte("old\r\nfile\r\n"))
	if err := ops.WriteFile(p, "new\nbody"); err != nil {
		t.Fatal(err)
	}
	if got, want := string(readRaw(t, p)), "new\r\nbody"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}