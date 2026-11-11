package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
