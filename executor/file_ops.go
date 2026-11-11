// Package executor implements the command handlers that run on the local machine.
package executor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// renameFile is the final step of an atomic write. It is a variable so tests
// can inject a failure between the temp-file write and the rename.
var renameFile = os.Rename

// binarySniffLen is how many leading bytes are scanned for a NUL byte when
// deciding whether a file is binary.
const binarySniffLen = 8192

// utf8BOM is the UTF-8 byte order mark.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// errBinaryEdit is the error str_replace / insert return for a binary or
// UTF-16 file.
const errBinaryEdit = "refusing to edit a binary or UTF-16 file; use runner_shell"

// FileOps handles all file system operations for the runner.
type FileOps struct {
	workspaceRoot string

	// pathLocks guards per-path mutual exclusion for file operations.
	// Without this, two concurrent tool calls targeting the SAME file
	// (e.g. the LLM issuing two str_replace calls on one path within a
	// single turn — engine.py's _run_tools_concurrently dispatches every
	// tool_use as an independent task with no per-path coordination) each
	// read the file, compute their edit against that snapshot, and write
	// back — the second write silently discards the first edit (classic
	// read-modify-write / TOCTOU race). mu protects the map itself; each
	// path gets its own reference-counted lock so unrelated files never
	// contend and entries are dropped once nobody uses them.
	mu        sync.Mutex
	pathLocks map[string]*pathLock
}

// pathLock is a reference-counted per-path mutex. refs counts the goroutines
// that hold or are waiting for it; the map entry is deleted when it drops to
// zero so the map does not grow with every distinct path ever touched.
type pathLock struct {
	mu   sync.Mutex
	refs int
}

// NewFileOps creates a FileOps scoped to the given workspace root.
func NewFileOps(workspaceRoot string) *FileOps {
	return &FileOps{
		workspaceRoot: filepath.Clean(workspaceRoot),
		pathLocks:     make(map[string]*pathLock),
	}
}

// foldPath returns s in the form used to compare paths: lower-cased on
// Windows (case-insensitive file system), unchanged elsewhere.
func foldPath(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(s)
	}
	return s
}

// lockKey is the key of the per-path lock map for a cleaned absolute path.
// Two spellings of the same Windows path must share one lock.
func lockKey(path string) string {
	return foldPath(path)
}

// lockPath acquires exclusive access to the given cleaned absolute path and
// returns a function that releases it. Callers must defer the returned
// function. Held for the full duration of a read-modify-write (or a plain
// write/delete) so no other goroutine can observe or clobber an in-flight
// change to the same file.
func (f *FileOps) lockPath(path string) func() {
	key := lockKey(path)
	f.mu.Lock()
	pl, ok := f.pathLocks[key]
	if !ok {
		pl = &pathLock{}
		f.pathLocks[key] = pl
	}
	pl.refs++
	f.mu.Unlock()

	pl.mu.Lock()
	return func() {
		pl.mu.Unlock()
		f.mu.Lock()
		pl.refs--
		if pl.refs == 0 {
			delete(f.pathLocks, key)
		}
		f.mu.Unlock()
	}
}

// guardPath validates that path is absolute and inside the workspace root.
// Returns the clean absolute path or an error. The containment check is
// purely lexical (symlinks are deliberately not resolved) and is
// case-insensitive on Windows.
func (f *FileOps) guardPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute: %s", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", path, err)
	}
	clean := filepath.Clean(abs)
	foldClean, foldRoot := foldPath(clean), foldPath(f.workspaceRoot)
	if foldClean != foldRoot && !strings.HasPrefix(foldClean, foldRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace root %q", path, f.workspaceRoot)
	}
	return clean, nil
}

// ReadFile returns the content of a file with line numbers, or a directory
// listing (2 levels) if path is a directory.
func (f *FileOps) ReadFile(path string, viewRange []int) (string, error) {
	clean, err := f.guardPath(path)
	if err != nil {
		return "", err
	}
	defer f.lockPath(clean)()

	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("path not found: %s", path)
	}

	if info.IsDir() {
		return f.listDir(clean)
	}
	return f.readFileLines(clean, viewRange)
}

func (f *FileOps) readFileLines(path string, viewRange []int) (string, error) {
	// An end <= 0 means "to the end of the file", so only a positive end can
	// be before the start.
	if len(viewRange) == 2 && viewRange[1] > 0 && viewRange[0] > viewRange[1] {
		return "", fmt.Errorf("invalid view_range: start %d is after end %d", viewRange[0], viewRange[1])
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}

	// Drop a UTF-8 BOM so it is never shown as U+FEFF, and normalize CRLF so
	// the LLM always sees clean lines regardless of OS line endings.
	data, _ = stripBOM(data)
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	// Remove trailing empty line that Split always adds for files ending in \n
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	start, end := 0, len(lines)
	if len(viewRange) == 2 {
		if viewRange[0] > 0 {
			start = viewRange[0] - 1 // convert 1-indexed to 0-indexed
		}
		if viewRange[1] > 0 && viewRange[1] < len(lines) {
			end = viewRange[1]
		}
		if start >= len(lines) {
			return "", fmt.Errorf("start line %d exceeds file length %d", viewRange[0], len(lines))
		}
	}

	var sb strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%d: %s\n", i+1, lines[i])
	}

	result := sb.String()
	const maxBytes = 500_000
	if len(result) > maxBytes {
		// Back up to a rune boundary so the clip never splits a multi-byte character.
		cut := maxBytes
		for i := 0; i < utf8.UTFMax && cut > 0 && !utf8.RuneStart(result[cut]); i++ {
			cut--
		}
		result = result[:cut] + "\n<response clipped>"
	}
	return result, nil
}

func (f *FileOps) listDir(path string) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("reading directory: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Directory: %s\n%s\n", path, strings.Repeat("=", 80))

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		fullPath := filepath.Join(path, name)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue
		}
		if info.IsDir() {
			fmt.Fprintf(&sb, "%s/\n", name)
			// One level of sub-entries
			subEntries, err := os.ReadDir(fullPath)
			if err == nil {
				shown := 0
				for _, se := range subEntries {
					if strings.HasPrefix(se.Name(), ".") {
						continue
					}
					suffix := ""
					if se.IsDir() {
						suffix = "/"
					}
					fmt.Fprintf(&sb, "  %s%s\n", se.Name(), suffix)
					shown++
					if shown >= 10 {
						remaining := 0
						for _, re := range subEntries {
							if !strings.HasPrefix(re.Name(), ".") {
								remaining++
							}
						}
						if remaining > shown {
							fmt.Fprintf(&sb, "  ... (%d items total)\n", remaining)
						}
						break
					}
				}
			}
		} else {
			fmt.Fprintf(&sb, "%s\n", name)
		}
	}
	return sb.String(), nil
}

// detectLineEnding reports whether data is predominantly CRLF. A file is CRLF
// iff it has strictly more "\r\n" sequences than bare "\n" (a "\n" not
// preceded by "\r"). Empty files, LF files and ties are LF.
func detectLineEnding(data []byte) (crlf bool) {
	crlfCount, bareLF := 0, 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		if i > 0 && data[i-1] == '\r' {
			crlfCount++
		} else {
			bareLF++
		}
	}
	return crlfCount > bareLF
}

// toDiskEndings converts LF-normalised text to the on-disk line-ending style.
func toDiskEndings(s string, crlf bool) string {
	if crlf {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// stripBOM removes a leading UTF-8 BOM and reports whether there was one.
func stripBOM(data []byte) ([]byte, bool) {
	if bytes.HasPrefix(data, utf8BOM) {
		return data[len(utf8BOM):], true
	}
	return data, false
}

// isBinaryOrUTF16 reports whether data must not be edited as text: it starts
// with a UTF-16 BOM or has a NUL byte within its first binarySniffLen bytes.
func isBinaryOrUTF16(data []byte) bool {
	if bytes.HasPrefix(data, []byte{0xFF, 0xFE}) || bytes.HasPrefix(data, []byte{0xFE, 0xFF}) {
		return true
	}
	head := data
	if len(head) > binarySniffLen {
		head = head[:binarySniffLen]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// withBOM prepends a UTF-8 BOM to s when the original file had one, unless s
// already starts with U+FEFF (so it is never doubled).
func withBOM(s string, hadBOM bool) string {
	if hadBOM && !strings.HasPrefix(s, "\uFEFF") {
		return "\uFEFF" + s
	}
	return s
}

// resolveWriteTarget returns the file an atomic write must replace. If path is
// a symlink the resolved target is returned so the link itself survives. The
// result is used only to pick the write destination, never to reject a path.
func resolveWriteTarget(path string) string {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
	}
	return path
}

// atomicWriteFile replaces dest with data without ever leaving a partially
// written file: the data is written and fsynced to a temp file in the same
// directory, the original permission bits are copied onto it (0644 for a new
// file), and it is renamed over dest. On any failure the temp file is removed
// and the original is untouched. If dest is a symlink, its target is replaced.
func atomicWriteFile(dest string, data []byte) (err error) {
	target := resolveWriteTarget(dest)
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(target); statErr == nil {
		mode = info.Mode().Perm()
	}

	base := filepath.Base(target)
	if len(base) > 100 {
		base = base[:100] // keep the temp name well under file-name limits
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+base+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting file mode: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err = renameWithRetry(tmpName, target); err != nil {
		return fmt.Errorf("replacing file: %w", err)
	}
	return nil
}

// renameWithRetry renames oldpath over newpath. On Windows a rename can fail
// transiently (sharing violation / access denied) while an editor or virus
// scanner briefly has the destination open, so a few short retries are made.
func renameWithRetry(oldpath, newpath string) error {
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = 5
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = renameFile(oldpath, newpath); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(time.Duration(10*(i+1)) * time.Millisecond)
		}
	}
	return err
}

// WriteFile creates or overwrites a file with the given content. See Create.
func (f *FileOps) WriteFile(path, content string) error {
	_, err := f.Create(path, content)
	return err
}

// Create creates or overwrites a file with the given content and reports
// whether an existing file was overwritten. Incoming content is normalized to
// LF internally. A new file is written as LF with no BOM; an existing file
// keeps its detected line-ending style (CRLF or LF) and its UTF-8 BOM, and
// content is written exactly as given otherwise (including its own final
// newline state).
func (f *FileOps) Create(path, content string) (overwritten bool, err error) {
	clean, err := f.guardPath(path)
	if err != nil {
		return false, err
	}
	defer f.lockPath(clean)()

	if err := os.MkdirAll(filepath.Dir(clean), 0755); err != nil {
		return false, fmt.Errorf("creating parent directories: %w", err)
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	crlf, hadBOM := false, false
	if existing, err := os.ReadFile(clean); err == nil {
		overwritten = true
		crlf = detectLineEnding(existing)
		_, hadBOM = stripBOM(existing)
	}
	out := withBOM(toDiskEndings(content, crlf), hadBOM)
	if err := atomicWriteFile(clean, []byte(out)); err != nil {
		return false, err
	}
	return overwritten, nil
}

// StrReplace replaces exactly one unique occurrence of oldStr with newStr.
func (f *FileOps) StrReplace(path, oldStr, newStr string) (string, error) {
	clean, err := f.guardPath(path)
	if err != nil {
		return "", err
	}
	if oldStr == "" {
		return "", fmt.Errorf("old_str must not be empty")
	}
	defer f.lockPath(clean)()

	data, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", path)
	}
	if isBinaryOrUTF16(data) {
		return "", errors.New(errBinaryEdit)
	}
	body, hadBOM := stripBOM(data)
	// Normalize CRLF in the file and in both operands so matches succeed
	// regardless of whether the LLM or the file uses CRLF or LF.
	content := strings.ReplaceAll(string(body), "\r\n", "\n")
	oldStr = strings.ReplaceAll(oldStr, "\r\n", "\n")
	newStr = strings.ReplaceAll(newStr, "\r\n", "\n")
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", fmt.Errorf("the specified text was not found in the file")
	}
	if count > 1 {
		return "", fmt.Errorf("found %d occurrences of the specified text — must be unique", count)
	}
	result := strings.Replace(content, oldStr, newStr, 1)
	out := withBOM(toDiskEndings(result, detectLineEnding(data)), hadBOM)
	if err := atomicWriteFile(clean, []byte(out)); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}
	return "Successfully replaced text at exactly one location.", nil
}

// Insert inserts text after the given 1-indexed line number.
func (f *FileOps) Insert(path string, lineNum int, newStr string) (string, error) {
	clean, err := f.guardPath(path)
	if err != nil {
		return "", err
	}
	defer f.lockPath(clean)()

	data, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", path)
	}
	if isBinaryOrUTF16(data) {
		return "", errors.New(errBinaryEdit)
	}
	body, hadBOM := stripBOM(data)

	// Normalize CRLF before splitting so inserted content aligns correctly.
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	if lineNum < 0 || lineNum > len(lines) {
		return "", fmt.Errorf("invalid line number %d (file has %d lines)", lineNum, len(lines))
	}

	// The inserted text becomes its own element(s) in the line list and the
	// Join below supplies the separators, so one trailing newline on newStr is
	// dropped — otherwise it would add a spurious blank line and change the
	// file's final-newline state.
	text := strings.TrimSuffix(strings.ReplaceAll(newStr, "\r\n", "\n"), "\n")

	newLines := make([]string, 0, len(lines)+1)
	newLines = append(newLines, lines[:lineNum]...)
	newLines = append(newLines, text)
	newLines = append(newLines, lines[lineNum:]...)

	out := withBOM(toDiskEndings(strings.Join(newLines, "\n"), detectLineEnding(data)), hadBOM)
	if err := atomicWriteFile(clean, []byte(out)); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}
	return fmt.Sprintf("Successfully inserted text after line %d.", lineNum), nil
}

// DeleteFile removes a file.
func (f *FileOps) DeleteFile(path string) error {
	clean, err := f.guardPath(path)
	if err != nil {
		return err
	}
	defer f.lockPath(clean)()

	return os.Remove(clean)
}
