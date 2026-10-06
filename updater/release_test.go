package updater

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTagFromLocation(t *testing.T) {
	good := map[string]string{
		"https://github.com/kevin4885/vectrify-agent-runner/releases/tag/v1.0.23":     "v1.0.23",
		"https://github.com/kevin4885/vectrify-agent-runner/releases/tag/v1.0.23?x=1": "v1.0.23",
		"https://github.com/kevin4885/vectrify-agent-runner/releases/tag/v1.0.23/":    "v1.0.23",
		"/kevin4885/vectrify-agent-runner/releases/tag/v2.0.0-rc1":                    "v2.0.0-rc1",
	}
	for in, want := range good {
		got, err := tagFromLocation(in)
		if err != nil || got != want {
			t.Errorf("tagFromLocation(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "https://github.com/x/y/releases", "https://github.com/x/y/releases/tag/", "https://example.com/login"} {
		if _, err := tagFromLocation(bad); err == nil {
			t.Errorf("tagFromLocation(%q) should fail", bad)
		}
	}
}

// fetcherFor points a releaseFetcher at a fake github.com / api.github.com.
func fetcherFor(web, api *httptest.Server) *releaseFetcher {
	f := newReleaseFetcher("o/r", "9.9.9")
	f.webBase = web.URL
	f.apiURL = api.URL + "/repos/o/r/releases/latest"
	return f
}

func TestFetcher_UsesRedirect_NoAPICall(t *testing.T) {
	var apiHits atomic.Int32
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/r/releases/latest" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "vectrify-agent-runner/") {
			t.Errorf("missing/odd User-Agent %q", r.Header.Get("User-Agent"))
		}
		http.Redirect(w, r, "/o/r/releases/tag/v1.2.3", http.StatusFound)
	}))
	defer web.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { apiHits.Add(1) }))
	defer api.Close()

	rel, err := fetcherFor(web, api).fetch()
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("tag = %q", rel.TagName)
	}
	if apiHits.Load() != 0 {
		t.Errorf("the rate-limited API was called %d times; the redirect route must not touch it", apiHits.Load())
	}
	// asset URLs must be the ones apply() looks up by name.
	for _, name := range []string{"checksums.txt", "vectrify-runner-windows-amd64.exe", "vectrify-runner-linux-arm64", "vectrify-runner-darwin-arm64"} {
		u, err := assetURL(rel.Assets, name)
		if err != nil {
			t.Errorf("asset %s missing: %v", name, err)
			continue
		}
		if want := web.URL + "/o/r/releases/download/v1.2.3/" + name; u != want {
			t.Errorf("asset %s url = %q, want %q", name, u, want)
		}
	}
}

func TestFetcher_RedirectFails_FallsBackToAPI(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer web.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v3.0.0","assets":[{"name":"checksums.txt","browser_download_url":"http://x/c"}]}`))
	}))
	defer api.Close()

	rel, err := fetcherFor(web, api).fetch()
	if err != nil || rel.TagName != "v3.0.0" || len(rel.Assets) != 1 {
		t.Fatalf("fallback result = %+v, %v", rel, err)
	}
}

func TestFetcher_NoReleases_200Page_IsAnErrorNotAVersion(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>releases list</html>")) // 200, no redirect
	}))
	defer web.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer api.Close()
	if rel, err := fetcherFor(web, api).fetch(); err == nil {
		t.Fatalf("expected an error, got release %+v", rel)
	}
}

func TestFetcher_BothFail_ReportsBoth_AndRateLimitIsNamed(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) }))
	defer web.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1791316743")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer api.Close()
	_, err := fetcherFor(web, api).fetch()
	if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error should mention both failures, got: %v", err)
	}
}

func TestFetcher_FailureLogThrottle(t *testing.T) {
	f := newReleaseFetcher("o/r", "1")
	if f.failures != 0 {
		t.Fatal("start at 0")
	}
	for i := 0; i < 13; i++ {
		f.noteFailure(quietLog(), http.ErrAbortHandler)
	}
	if f.failures != 13 {
		t.Fatalf("failures = %d", f.failures)
	}
	f.noteSuccess(quietLog())
	if f.failures != 0 {
		t.Fatal("success must reset the counter")
	}
}
