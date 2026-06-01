// Package updater checks for new releases on GitHub and self-updates the binary.
//
// On startup and every hour the runner calls the GitHub releases API.
// If a newer version is found it:
//   1. Downloads the binary for the current platform.
//   2. Downloads the SHA256 manifest and verifies the download before touching
//      the running binary.  The update is aborted if the checksum does not match.
//   3. Calls drain() to let in-flight commands finish (up to drainTimeout).
//   4. Writes a tiny update script, spawns it detached, and exits cleanly.
//      The script stops the service, swaps the binary, and restarts.
//
// Disabled automatically for dev builds (version == "dev").
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

const (
	githubRepo    = "kevin4885/vectrify-agent-runner"
	checkInterval = 1 * time.Hour
	apiURL        = "https://api.github.com/repos/" + githubRepo + "/releases/latest"

	// drainTimeout is the maximum time we wait for in-flight commands to finish
	// before applying an update and exiting.  Keeps updates snappy while still
	// giving short-running commands a chance to complete.
	drainTimeout = 30 * time.Second
)

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Start launches the background auto-update loop.
// drain is called with drainTimeout before the process exits to let in-flight
// commands finish.  Pass nil to skip draining (e.g. in tests).
// Returns immediately; the check runs in a goroutine.
// Does nothing for dev builds (version == "dev").
func Start(currentVersion string, log *slog.Logger, drain func(time.Duration)) {
	if currentVersion == "dev" {
		log.Debug("auto-update: disabled in dev build")
		return
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("updater panic recovered",
					"panic", p,
					"stack", string(debug.Stack()),
				)
			}
		}()
		checkAndApply(currentVersion, log, drain)
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for range ticker.C {
			checkAndApply(currentVersion, log, drain)
		}
	}()
}

func checkAndApply(currentVersion string, log *slog.Logger, drain func(time.Duration)) {
	rel, err := fetchLatestRelease()
	if err != nil {
		log.Warn("auto-update: version check failed", "err", err)
		return
	}
	latest := rel.TagName
	if len(latest) > 0 && latest[0] == 'v' {
		latest = latest[1:]
	}
	if !isNewer(currentVersion, latest) {
		log.Debug("auto-update: up to date", "version", currentVersion)
		return
	}
	log.Info("auto-update: new version available", "current", currentVersion, "latest", latest)
	if err := apply(latest, rel.Assets, log, drain); err != nil {
		log.Error("auto-update: failed", "err", err)
	}
}

func fetchLatestRelease() (*githubRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("fetching release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decoding release: %w", err)
	}
	return &rel, nil
}

// isNewer returns true if latest is a higher semver than current.
func isNewer(current, latest string) bool {
	if current == latest {
		return false
	}
	var cMaj, cMin, cPat int
	var lMaj, lMin, lPat int
	fmt.Sscanf(current, "%d.%d.%d", &cMaj, &cMin, &cPat)
	fmt.Sscanf(latest, "%d.%d.%d", &lMaj, &lMin, &lPat)
	if lMaj != cMaj {
		return lMaj > cMaj
	}
	if lMin != cMin {
		return lMin > cMin
	}
	return lPat > cPat
}

// assetURL returns the browser download URL for a named asset from the release,
// or an error if the asset is not present.
func assetURL(assets []githubAsset, name string) (string, error) {
	for _, a := range assets {
		if a.Name == name {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("asset %q not found in release", name)
}

// downloadFile downloads url and writes it to dest, replacing any existing file.
func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	return nil
}

// downloadSHA256Manifest fetches the checksum file and returns a map of
// filename -> expected hex SHA256.  The manifest is expected to be in the
// standard `sha256sum` format:
//
//	<hex>  <filename>
func downloadSHA256Manifest(url string) (map[string]string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("manifest download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		// sha256sum uses "  filename" (two spaces) or " *filename" (space+asterisk)
		name := strings.TrimPrefix(parts[1], "*")
		sums[name] = parts[0]
	}
	return sums, nil
}

// verifySHA256 computes the SHA256 of the file at path and compares it to
// expected (hex string).  Returns an error if they do not match.
func verifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening file for checksum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing file: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("SHA256 mismatch: got %s, expected %s", got, expected)
	}
	return nil
}
