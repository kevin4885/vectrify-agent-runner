// Package updater checks for new releases on GitHub and self-updates the binary.
//
// On startup and every 24 hours the runner calls the GitHub releases API.
// If a newer version is found it downloads the binary for the current platform,
// writes a tiny update script, spawns it detached, and exits cleanly.
// The script stops the service, swaps the binary, and restarts.
//
// Disabled automatically for dev builds (version == "dev").
package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"
)

const (
	githubRepo    = "kevin4885/vectrify-agent-runner"
	checkInterval = 24 * time.Hour
	apiURL        = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
}

// Start launches the background auto-update loop.
// Returns immediately; the check runs in a goroutine.
// Does nothing for dev builds (version == "dev").
func Start(currentVersion string, log *slog.Logger) {
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
		checkAndApply(currentVersion, log)
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for range ticker.C {
			checkAndApply(currentVersion, log)
		}
	}()
}

func checkAndApply(currentVersion string, log *slog.Logger) {
	latest, err := fetchLatestVersion()
	if err != nil {
		log.Warn("auto-update: version check failed", "err", err)
		return
	}
	if !isNewer(currentVersion, latest) {
		log.Debug("auto-update: up to date", "version", currentVersion)
		return
	}
	log.Info("auto-update: new version available", "current", currentVersion, "latest", latest)
	if err := apply(latest, log); err != nil {
		log.Error("auto-update: failed", "err", err)
	}
}

func fetchLatestVersion() (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("fetching release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("decoding release: %w", err)
	}
	v := rel.TagName
	if len(v) > 0 && v[0] == 'v' {
		v = v[1:]
	}
	return v, nil
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
