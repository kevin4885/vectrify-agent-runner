package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newTransferRoot creates a temp directory to use as a workspace root for tests.
func newTransferRoot(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	return dir, func() { os.RemoveAll(dir) }
}

// sha256Hex returns the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// newDownloadServer returns an httptest.Server that serves body for GET requests.
// The URL scheme is http:// (loopback), so tests must bypass the https-only check
// by calling downloadFile / uploadFile directly rather than TransferFile.
func newDownloadServer(t *testing.T, body []byte, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		if statusCode >= 200 && statusCode < 300 {
			w.Write(body)
		}
	}))
}

// newUploadServer returns an httptest.Server that records the body of PUT requests.
func newUploadServer(t *testing.T, statusCode int) (*httptest.Server, *[]byte) {
	t.Helper()
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		received = data
		w.WriteHeader(statusCode)
	}))
	return srv, &received
}

// ─────────────────────────────────────────────────────────────────────────────
// validateTransferURL
// ─────────────────────────────────────────────────────────────────────────────

func TestValidateTransferURL_HTTPS(t *testing.T) {
	if err := validateTransferURL("https://bucket.s3.amazonaws.com/key?X-Amz-Signature=abc"); err != nil {
		t.Fatalf("expected no error for https URL, got: %v", err)
	}
}

func TestValidateTransferURL_HTTP_Rejected(t *testing.T) {
	if err := validateTransferURL("http://example.com/file"); err == nil {
		t.Fatal("expected error for http:// URL, got nil")
	}
}

func TestValidateTransferURL_NoScheme_Rejected(t *testing.T) {
	if err := validateTransferURL("example.com/file"); err == nil {
		t.Fatal("expected error for URL without scheme, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TransferFile — path containment (HTTPS check bypassed at the wrapper level)
// ─────────────────────────────────────────────────────────────────────────────

func TestTransferFile_PathOutsideRoot_Download(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	// Path that escapes the workspace root.
	outsidePath := filepath.Join(os.TempDir(), "evil.txt")

	// We use a valid-looking https URL; containment check fires before any HTTP.
	_, err := TransferFile(root, "download", "https://example.com/file", outsidePath, 1024, false)
	if err == nil {
		t.Fatal("expected error for path outside workspace root, got nil")
	}
	if !strings.Contains(err.Error(), "outside the workspace root") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTransferFile_PathOutsideRoot_Upload(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	outsidePath := filepath.Join(os.TempDir(), "evil.txt")

	_, err := TransferFile(root, "upload", "https://example.com/file", outsidePath, 1024, false)
	if err == nil {
		t.Fatal("expected error for path outside workspace root, got nil")
	}
	if !strings.Contains(err.Error(), "outside the workspace root") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTransferFile_NonHTTPS_Rejected(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	dest := filepath.Join(root, "file.txt")
	_, err := TransferFile(root, "download", "http://example.com/file", dest, 1024, false)
	if err == nil {
		t.Fatal("expected error for non-https URL, got nil")
	}
	if !strings.Contains(err.Error(), "https://") {
		t.Errorf("expected 'https://' in error, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// downloadFile — happy path
// ─────────────────────────────────────────────────────────────────────────────

func TestDownloadFile_HappyPath(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	content := []byte("hello, world! this is test content for download.")
	srv := newDownloadServer(t, content, http.StatusOK)
	defer srv.Close()

	destPath := filepath.Join(root, "downloaded.txt")
	result, err := downloadFile(srv.URL, destPath, 1024*1024, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check bytes
	if result.Bytes != int64(len(content)) {
		t.Errorf("bytes: got %d, want %d", result.Bytes, len(content))
	}

	// Check SHA256
	want := sha256Hex(content)
	if result.SHA256 != want {
		t.Errorf("sha256: got %q, want %q", result.SHA256, want)
	}

	// Check file on disk
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file content mismatch: got %q, want %q", got, content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// downloadFile — overwrite semantics
// ─────────────────────────────────────────────────────────────────────────────

func TestDownloadFile_ExistingFile_OverwriteFalse_Error(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	destPath := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(destPath, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := newDownloadServer(t, []byte("new content"), http.StatusOK)
	defer srv.Close()

	_, err := downloadFile(srv.URL, destPath, 1024*1024, false)
	if err == nil {
		t.Fatal("expected error when file exists and overwrite=false, got nil")
	}
	if !strings.Contains(err.Error(), "overwrite=true") {
		t.Errorf("unexpected error message: %v", err)
	}

	// Original file must be unchanged.
	got, _ := os.ReadFile(destPath)
	if string(got) != "original" {
		t.Errorf("original file was modified: %q", got)
	}
}

func TestDownloadFile_ExistingFile_OverwriteTrue_Replaces(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	destPath := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(destPath, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	newContent := []byte("replaced content")
	srv := newDownloadServer(t, newContent, http.StatusOK)
	defer srv.Close()

	result, err := downloadFile(srv.URL, destPath, 1024*1024, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Bytes != int64(len(newContent)) {
		t.Errorf("bytes mismatch: got %d, want %d", result.Bytes, len(newContent))
	}

	got, _ := os.ReadFile(destPath)
	if !bytes.Equal(got, newContent) {
		t.Errorf("file not replaced: got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// downloadFile — size limits
// ─────────────────────────────────────────────────────────────────────────────

func TestDownloadFile_ContentLengthOverLimit_Error(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	// Server sends Content-Length header that exceeds maxBytes.
	bigContent := bytes.Repeat([]byte("x"), 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bigContent)))
		w.WriteHeader(http.StatusOK)
		w.Write(bigContent)
	}))
	defer srv.Close()

	destPath := filepath.Join(root, "too-big.txt")
	maxBytes := int64(100) // much smaller than the content
	_, err := downloadFile(srv.URL, destPath, maxBytes, false)
	if err == nil {
		t.Fatal("expected error for oversize Content-Length, got nil")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("unexpected error: %v", err)
	}

	// No partial file should remain.
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Error("partial file should have been removed after size-limit error")
	}
}

func TestDownloadFile_ChunkedBodyOverLimit_Error(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	// Server does NOT send Content-Length — chunked transfer encoding.
	bigContent := bytes.Repeat([]byte("y"), 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately omit Content-Length so the pre-check is skipped.
		w.WriteHeader(http.StatusOK)
		w.Write(bigContent)
	}))
	defer srv.Close()

	destPath := filepath.Join(root, "too-big-chunked.txt")
	maxBytes := int64(100) // much smaller than the content
	_, err := downloadFile(srv.URL, destPath, maxBytes, false)
	if err == nil {
		t.Fatal("expected error for oversized chunked body, got nil")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("unexpected error: %v", err)
	}

	// No partial file should remain.
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Error("partial file should have been removed after chunked size-limit error")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// downloadFile — non-2xx response
// ─────────────────────────────────────────────────────────────────────────────

func TestDownloadFile_Non2xx_Error_NoFile(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	srv := newDownloadServer(t, nil, http.StatusForbidden)
	defer srv.Close()

	destPath := filepath.Join(root, "forbidden.txt")
	_, err := downloadFile(srv.URL, destPath, 1024*1024, false)
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected status code in error, got: %v", err)
	}
	// URL must not appear in the error message.
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("presigned URL leaked into error message: %v", err)
	}

	// No file should have been created.
	if _, statErr := os.Stat(destPath); !os.IsNotExist(statErr) {
		t.Error("file should not exist after failed download")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// uploadFile — happy path
// ─────────────────────────────────────────────────────────────────────────────

func TestUploadFile_HappyPath(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	content := []byte("upload test content — binary-safe bytes: \x00\x01\x02\x03")
	srcPath := filepath.Join(root, "upload-me.bin")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	srv, received := newUploadServer(t, http.StatusOK)
	defer srv.Close()

	result, err := uploadFile(srv.URL, srcPath, 1024*1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Bytes and SHA256
	if result.Bytes != int64(len(content)) {
		t.Errorf("bytes: got %d, want %d", result.Bytes, len(content))
	}
	want := sha256Hex(content)
	if result.SHA256 != want {
		t.Errorf("sha256: got %q, want %q", result.SHA256, want)
	}

	// Server received the exact bytes.
	if !bytes.Equal(*received, content) {
		t.Errorf("server received wrong bytes: got %q, want %q", *received, content)
	}
}

func TestUploadFile_ContentLengthSet(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	content := []byte("check content-length")
	srcPath := filepath.Join(root, "cl-test.txt")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	var gotCL int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCL = r.ContentLength
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := uploadFile(srv.URL, srcPath, 1024*1024); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCL != int64(len(content)) {
		t.Errorf("ContentLength: got %d, want %d", gotCL, len(content))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// uploadFile — error cases
// ─────────────────────────────────────────────────────────────────────────────

func TestUploadFile_MissingFile_Error(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	srv, _ := newUploadServer(t, http.StatusOK)
	defer srv.Close()

	_, err := uploadFile(srv.URL, filepath.Join(root, "ghost.txt"), 1024*1024)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUploadFile_Directory_Error(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	srv, _ := newUploadServer(t, http.StatusOK)
	defer srv.Close()

	_, err := uploadFile(srv.URL, root, 1024*1024)
	if err == nil {
		t.Fatal("expected error for directory path, got nil")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUploadFile_OverSize_Error(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	content := bytes.Repeat([]byte("z"), 1024)
	srcPath := filepath.Join(root, "big.txt")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newUploadServer(t, http.StatusOK)
	defer srv.Close()

	_, err := uploadFile(srv.URL, srcPath, 100) // limit much smaller than file
	if err == nil {
		t.Fatal("expected error for oversize upload, got nil")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUploadFile_Non2xx_Error(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	content := []byte("will be rejected")
	srcPath := filepath.Join(root, "rejected.txt")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newUploadServer(t, http.StatusForbidden)
	defer srv.Close()

	_, err := uploadFile(srv.URL, srcPath, 1024*1024)
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected status code in error, got: %v", err)
	}
	// URL must not appear in the error message.
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("presigned URL leaked into error message: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// downloadFile — parent directory creation
// ─────────────────────────────────────────────────────────────────────────────

func TestDownloadFile_CreatesParentDirs(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	content := []byte("nested file content")
	srv := newDownloadServer(t, content, http.StatusOK)
	defer srv.Close()

	destPath := filepath.Join(root, "sub", "dir", "nested.txt")
	if _, err := downloadFile(srv.URL, destPath, 1024*1024, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(destPath); err != nil {
		t.Errorf("nested file not created: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Network-error path — presigned URL must NOT appear in error message
// ─────────────────────────────────────────────────────────────────────────────

// TestDownloadFile_NetworkError_NoURLLeak verifies that when the HTTP GET itself
// fails (e.g. connection refused), the returned error does not embed the URL.
// This is critical because the URL is a presigned bearer credential.
func TestDownloadFile_NetworkError_NoURLLeak(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	// Use a known-unreachable address (server never started).
	unreachableURL := "http://127.0.0.1:1" // port 1 — always refused
	destPath := filepath.Join(root, "never.txt")

	_, err := downloadFile(unreachableURL, destPath, 1024*1024, false)
	if err == nil {
		t.Fatal("expected error for unreachable host, got nil")
	}
	errMsg := err.Error()
	// The URL must NOT appear verbatim in the error text.
	if strings.Contains(errMsg, unreachableURL) {
		t.Errorf("presigned URL leaked into download error: %q", errMsg)
	}
}

// TestUploadFile_NetworkError_NoURLLeak verifies the same for PUT.
func TestUploadFile_NetworkError_NoURLLeak(t *testing.T) {
	root, cleanup := newTransferRoot(t)
	defer cleanup()

	content := []byte("content")
	srcPath := filepath.Join(root, "src.txt")
	if err := os.WriteFile(srcPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	unreachableURL := "http://127.0.0.1:1"
	_, err := uploadFile(unreachableURL, srcPath, 1024*1024)
	if err == nil {
		t.Fatal("expected error for unreachable host, got nil")
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, unreachableURL) {
		t.Errorf("presigned URL leaked into upload error: %q", errMsg)
	}
}
