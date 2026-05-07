// Package executor implements the command handlers that run on the local machine.
package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileOps handles all file system operations for the runner.
type FileOps struct {
	workspaceRoot string
}

// NewFileOps creates a FileOps scoped to the given workspace root.
func NewFileOps(workspaceRoot string) *FileOps {
	return &FileOps{workspaceRoot: filepath.Clean(workspaceRoot)}
}

// guardPath validates that path is inside the workspace root.
// Returns the clean absolute path or an error.
func (f *FileOps) guardPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", path, err)
	}
	clean := filepath.Clean(abs)
	if clean != f.workspaceRoot && !strings.HasPrefix(clean, f.workspaceRoot+string(filepath.Separator)) {
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
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
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
		result = result[:maxBytes] + "\n<response clipped>"
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

// WriteFile creates or overwrites a file with the given content.
func (f *FileOps) WriteFile(path, content string) error {
	clean, err := f.guardPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0755); err != nil {
		return fmt.Errorf("creating parent directories: %w", err)
	}
	return os.WriteFile(clean, []byte(content), 0644)
}

// StrReplace replaces exactly one unique occurrence of oldStr with newStr.
func (f *FileOps) StrReplace(path, oldStr, newStr string) (string, error) {
	clean, err := f.guardPath(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", path)
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", fmt.Errorf("the specified text was not found in the file")
	}
	if count > 1 {
		return "", fmt.Errorf("found %d occurrences of the specified text — must be unique", count)
	}
	result := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(clean, []byte(result), 0644); err != nil {
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
	data, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", path)
	}

	lines := strings.Split(string(data), "\n")
	if lineNum < 0 || lineNum > len(lines) {
		return "", fmt.Errorf("invalid line number %d (file has %d lines)", lineNum, len(lines))
	}

	// Ensure the inserted line ends with \n if it is not the last line
	text := newStr
	if lineNum < len(lines) && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}

	newLines := make([]string, 0, len(lines)+1)
	newLines = append(newLines, lines[:lineNum]...)
	newLines = append(newLines, text)
	newLines = append(newLines, lines[lineNum:]...)

	if err := os.WriteFile(clean, []byte(strings.Join(newLines, "\n")), 0644); err != nil {
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
	return os.Remove(clean)
}
