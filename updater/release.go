package updater

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// releaseFetcher finds the latest release without spending GitHub API quota.
//
// WHY NOT JUST CALL THE API EVERY CHECK: the unauthenticated REST API allows
// 60 requests/hour per IP, and (verified against api.github.com) a conditional
// request that returns 304 STILL counts against it - only authenticated
// requests get free 304s. Checking every 5 minutes is 12 requests/hour per
// runner, so a handful of runners behind one office IP would exhaust the quota
// and every one of them would silently stop updating.
//
// THE FIX: https://github.com/<repo>/releases/latest is an ordinary web page
// that answers 302 -> .../releases/tag/<tag>. It is not part of the API quota.
// The tag is all that is needed: release assets live at the predictable URL
// .../releases/download/<tag>/<asset>, which is exactly what the browser
// download URL in the API response is. So the check is one redirect; no API
// call, no JSON, no quota. The API is only a FALLBACK for when the redirect
// route fails (e.g. a proxy that mangles it), so behaviour never gets worse.
//
// Only ever used from the single updater goroutine.
type releaseFetcher struct {
	repo      string
	webBase   string // https://github.com
	apiURL    string // fallback
	userAgent string

	noRedirect *http.Client // never follows redirects: the Location IS the answer
	api        *http.Client

	failures int // consecutive failed checks, for log throttling
}

// runnerAssetNames are the files an update needs for each platform; used to
// build download URLs from a tag. Kept next to the code that downloads them
// (apply_*.go) by name only - a missing asset 404s at download time, which is
// reported exactly like the old "asset not found in release" case.
var runnerAssetNames = []string{
	"checksums.txt",
	"vectrify-runner-windows-amd64.exe",
	"vectrify-runner-linux-amd64",
	"vectrify-runner-linux-arm64",
	"vectrify-runner-darwin-amd64",
	"vectrify-runner-darwin-arm64",
}

func newReleaseFetcher(repo, version string) *releaseFetcher {
	return &releaseFetcher{
		repo:      repo,
		webBase:   "https://github.com",
		apiURL:    "https://api.github.com/repos/" + repo + "/releases/latest",
		userAgent: "vectrify-agent-runner/" + version,
		noRedirect: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		api: &http.Client{Timeout: 15 * time.Second},
	}
}

func (f *releaseFetcher) fetch() (*githubRelease, error) {
	rel, err := f.fetchViaRedirect()
	if err == nil {
		return rel, nil
	}
	apiRel, apiErr := f.fetchViaAPI()
	if apiErr == nil {
		return apiRel, nil
	}
	return nil, fmt.Errorf("%v; API fallback also failed: %w", err, apiErr)
}

// fetchViaRedirect: GET /releases/latest, read the tag from Location.
func (f *releaseFetcher) fetchViaRedirect() (*githubRelease, error) {
	req, err := http.NewRequest(http.MethodGet, f.webBase+"/"+f.repo+"/releases/latest", nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	resp, err := f.noRedirect.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching latest release page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		// 200 here means the repo has no releases (GitHub serves the releases
		// list); anything else is an error. Either way: no usable answer.
		return nil, fmt.Errorf("latest release page returned HTTP %d, expected a redirect", resp.StatusCode)
	}
	tag, err := tagFromLocation(resp.Header.Get("Location"))
	if err != nil {
		return nil, err
	}
	return f.releaseForTag(tag), nil
}

// tagFromLocation extracts "v1.2.3" from ".../releases/tag/v1.2.3".
func tagFromLocation(loc string) (string, error) {
	const marker = "/releases/tag/"
	i := strings.LastIndex(loc, marker)
	if i < 0 {
		return "", fmt.Errorf("unexpected redirect target %q (no %s)", loc, marker)
	}
	tag := loc[i+len(marker):]
	if j := strings.IndexAny(tag, "?#/"); j >= 0 {
		tag = tag[:j]
	}
	if tag == "" {
		return "", fmt.Errorf("empty tag in redirect target %q", loc)
	}
	return tag, nil
}

// releaseForTag builds the release record the apply code expects, with
// predictable asset URLs instead of ones read from the API.
func (f *releaseFetcher) releaseForTag(tag string) *githubRelease {
	rel := &githubRelease{TagName: tag}
	for _, name := range runnerAssetNames {
		rel.Assets = append(rel.Assets, githubAsset{
			Name:               name,
			BrowserDownloadURL: f.webBase + "/" + f.repo + "/releases/download/" + tag + "/" + name,
		})
	}
	return rel
}

// fetchViaAPI is the fallback. Costs one unit of the 60/hour/IP quota.
func (f *releaseFetcher) fetchViaAPI() (*githubRelease, error) {
	req, err := http.NewRequest(http.MethodGet, f.apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building API request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent) // GitHub requires one
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := f.api.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching release: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var rel githubRelease
		if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
			return nil, fmt.Errorf("decoding release: %w", err)
		}
		return &rel, nil
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, fmt.Errorf("GitHub API rate limit exhausted (HTTP %d, resets at unix time %s)",
				resp.StatusCode, resp.Header.Get("X-RateLimit-Reset"))
		}
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	default:
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
}

// noteFailure records a failed check. A machine that is offline would
// otherwise log a WARN every 5 minutes forever: warn on the first failure and
// then only about once an hour (every 12th), debug in between.
func (f *releaseFetcher) noteFailure(log *slog.Logger, err error) {
	f.failures++
	if f.failures == 1 || f.failures%12 == 0 {
		log.Warn("auto-update: version check failed", "err", err, "consecutive_failures", f.failures)
		return
	}
	log.Debug("auto-update: version check failed", "err", err, "consecutive_failures", f.failures)
}

// noteSuccess records a successful check, announcing recovery after failures.
func (f *releaseFetcher) noteSuccess(log *slog.Logger) {
	if f.failures > 0 {
		log.Info("auto-update: version check working again", "after_failures", f.failures)
	}
	f.failures = 0
}
