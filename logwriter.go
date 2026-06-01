package main

import (
	"fmt"
	"os"
	"sync"
)

const (
	// logMaxBytes is the maximum size of the active log file before rotation.
	// Rotating at 10 MB keeps individual files small enough to read in a text
	// editor while retaining ~40 MB of history across four files.
	logMaxBytes = 10 * 1024 * 1024 // 10 MB

	// logBackups is the number of rotated files to keep (.1, .2, .3).
	// Combined with the active file that is 4 files * 10 MB = 40 MB max.
	logBackups = 3
)

// rotatingWriter is a thread-safe io.Writer that rotates the log file when it
// exceeds maxBytes.  Rotated files are named <path>.1, <path>.2, <path>.3.
// When a new rotation occurs, existing backups are shifted up by one and the
// oldest (.3) is deleted.  No external dependencies required.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
}

// newRotatingWriter opens (or creates) the log file at path and returns a
// rotatingWriter capped at maxBytes per file.
func newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	return &rotatingWriter{path: path, maxBytes: maxBytes, f: f, size: size}, nil
}

// Write implements io.Writer.  If adding p would exceed maxBytes the file is
// rotated first, then p is written to the fresh file.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxBytes {
		w.rotate() // best-effort; if rotate fails we keep writing to the old file
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the underlying file.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// rotate shifts backup files and opens a fresh active log file.
// Called with w.mu already held.
func (w *rotatingWriter) rotate() {
	w.f.Close()
	// Shift: .3 deleted, .2->.3, .1->.2, active->.1
	os.Remove(fmt.Sprintf("%s.%d", w.path, logBackups))
	for i := logBackups - 1; i >= 1; i-- {
		os.Rename(
			fmt.Sprintf("%s.%d", w.path, i),
			fmt.Sprintf("%s.%d", w.path, i+1),
		)
	}
	os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		// Rotation failed — reopen the original path and keep writing.
		// This prevents a total log blackout if the filesystem is temporarily
		// unavailable (e.g. network drive).
		f, _ = os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	}
	w.f = f
	w.size = 0
}
