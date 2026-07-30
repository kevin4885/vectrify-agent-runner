package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TransferResult holds the outcome of a successful file transfer.
type TransferResult struct {
	Bytes  int64
	SHA256 string
}

// httpClient is the shared HTTP client used for all file transfers.
// Timeout is set to 280 s — slightly under the 300 s presigned-URL expiry and
// the API-side command timeout — so the runner fails cleanly before both sides
// give up.
var httpClient = &http.Client{Timeout: 280 * time.Second}

// validateTransferURL returns an error if url is not an HTTPS URL.
// This is a standalone function so tests can call it directly to cover the
// scheme check without needing a real HTTP server.
func validateTransferURL(rawURL string) error {
	if !strings.HasPrefix(rawURL, "https://") {
		return fmt.Errorf("transfer URL must use https:// (got scheme %q)", schemeOf(rawURL))
	}
	return nil
}

func schemeOf(rawURL string) string {
	if idx := strings.Index(rawURL, "://"); idx >= 0 {
		return rawURL[:idx]
	}
	return "(none)"
}

// TransferFile copies a file between the runner filesystem and an S3 presigned URL.
//
// direction must be "download" (S3 → runner) or "upload" (runner → S3).
// workspaceRoot is the configured workspace root; path is validated to be inside it.
// url must be https:// (presigned GET for download, presigned PUT for upload).
// maxBytes is the maximum permitted file size in bytes (checked before/during transfer).
// overwrite controls whether an existing destination file is replaced (download only).
//
// The presigned URL is never logged — it is a bearer credential.
func TransferFile(workspaceRoot, direction, url, path string, maxBytes int64, overwrite bool) (TransferResult, error) {
	// ── URL validation ─────────────────────────────────────────────────────────
	if err := validateTransferURL(url); err != nil {
		return TransferResult{}, err
	}

	// ── Path containment (reuse file_ops guardPath logic) ─────────────────────
	if path == "" {
		return TransferResult{}, fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return TransferResult{}, fmt.Errorf("invalid path %q: %w", path, err)
	}
	clean := filepath.Clean(abs)
	root := filepath.Clean(workspaceRoot)
	if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return TransferResult{}, fmt.Errorf("path %q is outside the workspace root %q", path, workspaceRoot)
	}

	switch direction {
	case "download":
		return downloadFile(url, clean, maxBytes, overwrite)
	case "upload":
		return uploadFile(url, clean, maxBytes)
	default:
		return TransferResult{}, fmt.Errorf("unknown direction %q: must be \"download\" or \"upload\"", direction)
	}
}

// downloadFile fetches url and writes it to destPath.
// It streams through a temp file in the same directory for atomic replacement.
func downloadFile(url, destPath string, maxBytes int64, overwrite bool) (TransferResult, error) {
	// ── Overwrite guard ────────────────────────────────────────────────────────
	if _, err := os.Stat(destPath); err == nil {
		if !overwrite {
			return TransferResult{}, fmt.Errorf("file exists; pass overwrite=true to replace it")
		}
	}

	// ── Create parent directories ──────────────────────────────────────────────
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return TransferResult{}, fmt.Errorf("creating parent directories: %w", err)
	}

	// ── HTTP GET ───────────────────────────────────────────────────────────────
	resp, err := httpClient.Get(url) //nolint:noctx // presigned URL already encodes credentials
	if err != nil {
		// Do NOT wrap err directly — *url.Error embeds the full presigned URL.
		return TransferResult{}, fmt.Errorf("HTTP GET request failed (network or TLS error)")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TransferResult{}, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Pre-check Content-Length when the server provides it.
	if resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return TransferResult{}, fmt.Errorf(
			"file size %d bytes exceeds the %d-byte limit", resp.ContentLength, maxBytes,
		)
	}

	// ── Write to temp file in the same directory (atomic rename) ──────────────
	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), ".vectrify-transfer-*")
	if err != nil {
		return TransferResult{}, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	// Ensure temp is removed on any error path.
	success := false
	defer func() {
		if !success {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	// ── Stream, count, and hash ────────────────────────────────────────────────
	h := sha256.New()
	written, err := streamWithLimit(tmpFile, h, resp.Body, maxBytes)
	if err != nil {
		return TransferResult{}, err
	}
	if err := tmpFile.Close(); err != nil {
		return TransferResult{}, fmt.Errorf("flushing temp file: %w", err)
	}

	// ── Atomic rename ──────────────────────────────────────────────────────────
	if err := os.Rename(tmpPath, destPath); err != nil {
		return TransferResult{}, fmt.Errorf("renaming temp file: %w", err)
	}

	success = true
	return TransferResult{
		Bytes:  written,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// uploadFile reads the file at srcPath and HTTP-PUTs it to url.
func uploadFile(url, srcPath string, maxBytes int64) (TransferResult, error) {
	// ── Source file checks ─────────────────────────────────────────────────────
	info, err := os.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return TransferResult{}, fmt.Errorf("file not found: %s", srcPath)
		}
		return TransferResult{}, fmt.Errorf("stat %s: %w", srcPath, err)
	}
	if info.IsDir() {
		return TransferResult{}, fmt.Errorf("path is a directory; only files can be uploaded: %s", srcPath)
	}
	if info.Size() > maxBytes {
		return TransferResult{}, fmt.Errorf(
			"file size %d bytes exceeds the %d-byte limit", info.Size(), maxBytes,
		)
	}

	// ── Open and wrap with hashing tee ────────────────────────────────────────
	f, err := os.Open(srcPath)
	if err != nil {
		return TransferResult{}, fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	tee := io.TeeReader(f, h)

	// ── HTTP PUT ───────────────────────────────────────────────────────────────
	req, err := http.NewRequest(http.MethodPut, url, tee)
	if err != nil {
		return TransferResult{}, fmt.Errorf("building PUT request: %w", err)
	}
	req.ContentLength = info.Size()
	// No Content-Type header — the presign is issued without a content-type
	// constraint so the runner does not need to guess the MIME type.

	resp, err := httpClient.Do(req)
	if err != nil {
		// Do NOT wrap err directly — *url.Error embeds the full presigned URL.
		return TransferResult{}, fmt.Errorf("HTTP PUT request failed (network or TLS error)")
	}
	defer resp.Body.Close()
	// Drain body to allow connection reuse, but discard it — body is not logged.
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TransferResult{}, fmt.Errorf("upload failed: HTTP %d", resp.StatusCode)
	}

	return TransferResult{
		Bytes:  info.Size(),
		SHA256: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// streamWithLimit copies from r to w (and to hashWriter) while counting bytes.
// Returns an error if the byte count would exceed maxBytes.
func streamWithLimit(w io.Writer, hashWriter io.Writer, r io.Reader, maxBytes int64) (int64, error) {
	mw := io.MultiWriter(w, hashWriter)
	// Read in chunks; abort as soon as the running total exceeds the limit.
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxBytes {
				return total, fmt.Errorf(
					"file exceeds the %d-byte limit (aborted after %d bytes)", maxBytes, total,
				)
			}
			if _, writeErr := mw.Write(buf[:n]); writeErr != nil {
				return total, fmt.Errorf("writing to temp file: %w", writeErr)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return total, fmt.Errorf("reading response body: %w", readErr)
		}
	}
	return total, nil
}
