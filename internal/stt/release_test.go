package stt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// mockReleaseHandler returns an http.Handler that serves a
// /repos/{owner}/{repo}/releases/latest JSON payload
// listing the supplied assets. Tests can construct a
// GitHubRelease with the assets they want to expose.
func mockReleaseHandler(t *testing.T, assets []GitHubAsset) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Crude path matcher — we only expect the latest
		// release endpoint from the resolver.
		if !strings.Contains(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubRelease{
			TagName: "v9.9.9-test",
			Name:    "test release",
			Assets:  assets,
		})
	}))
}

func TestGitHubWorkerResolverPicksCurrentPlatformAsset(t *testing.T) {
	wantName := "nightme-stt_v9.9.9-test_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		wantName += ".zip"
	} else {
		wantName += ".tar.gz"
	}
	wantURL := "https://example.invalid/" + wantName

	srv := mockReleaseHandler(t, []GitHubAsset{
		{Name: "other-platform.tar.gz", BrowserDownloadURL: "https://example.invalid/other"},
		{Name: wantName, BrowserDownloadURL: wantURL, Size: 4242, Digest: "sha256:abc123"},
	})

	r := &GitHubWorkerResolver{
		Repo:       "foo/bar",
		HttpClient: srv.Client(),
	}
	got, err := r.WorkerAsset(WithGitHubAPIRoot(t.Context(), srv.URL))
	if err != nil {
		t.Fatalf("WorkerAsset: %v", err)
	}
	if got.Name != wantName {
		t.Fatalf("name mismatch: got %q want %q", got.Name, wantName)
	}
	if got.URL != wantURL {
		t.Fatalf("url mismatch: got %q want %q", got.URL, wantURL)
	}
	if got.Tag != "v9.9.9-test" {
		t.Fatalf("tag mismatch: got %q", got.Tag)
	}
	if got.SHA256 != "abc123" {
		t.Fatalf("SHA from digest field: got %q want abc123", got.SHA256)
	}
	if runtime.GOOS == "windows" {
		if got.Archive != "zip" || got.Binary != "nightme-stt.exe" {
			t.Fatalf("windows archive/binary: got %s/%s", got.Archive, got.Binary)
		}
	} else {
		if got.Archive != "tar.gz" || got.Binary != "nightme-stt" {
			t.Fatalf("unix archive/binary: got %s/%s", got.Archive, got.Binary)
		}
	}
}

func TestGitHubWorkerResolverFallsBackToSha256Sidecar(t *testing.T) {
	wantName := "nightme-stt_v0.1.0_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		wantName += ".zip"
	} else {
		wantName += ".tar.gz"
	}
	// Two routes: the release API returns the asset WITHOUT
	// a digest field, and the .sha256 sidecar returns the hex.
	var assetURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v0.1.0",
				Assets: []GitHubAsset{
					{Name: wantName, BrowserDownloadURL: assetURL, Size: 1000},
				},
			})
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			_, _ = w.Write([]byte("deadbeefcafe\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	assetURL = srv.URL + "/" + wantName

	r := &GitHubWorkerResolver{
		Repo:       "foo/bar",
		HttpClient: srv.Client(),
	}
	got, err := r.WorkerAsset(WithGitHubAPIRoot(t.Context(), srv.URL))
	if err != nil {
		t.Fatalf("WorkerAsset: %v", err)
	}
	if got.SHA256 != "deadbeefcafe" {
		t.Fatalf("SHA from sidecar: got %q want deadbeefcafe", got.SHA256)
	}
}

func TestGitHubWorkerResolverReportsMissingPlatformAsset(t *testing.T) {
	srv := mockReleaseHandler(t, []GitHubAsset{
		{Name: "for-some-other-platform.tar.gz"},
	})
	r := &GitHubWorkerResolver{
		Repo:       "foo/bar",
		HttpClient: srv.Client(),
	}
	_, err := r.WorkerAsset(WithGitHubAPIRoot(t.Context(), srv.URL))
	if err == nil {
		t.Fatalf("expected missing-asset error")
	}
	if !strings.Contains(err.Error(), "no asset") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitHubWorkerResolverReportsRateLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	r := &GitHubWorkerResolver{
		Repo:       "foo/bar",
		HttpClient: srv.Client(),
	}
	_, err := r.WorkerAsset(WithGitHubAPIRoot(t.Context(), srv.URL))
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("expected rate limit error, got %v", err)
	}
}

func TestGitHubModelResolverPicksSenseVoiceAsset(t *testing.T) {
	wantName := "sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17-int8.tar.bz2"
	srv := mockReleaseHandler(t, []GitHubAsset{
		{Name: "sherpa-onnx-streaming-zipformer-bilingual.tar.bz2"},
		{Name: wantName, BrowserDownloadURL: "https://example.invalid/" + wantName, Size: 230000000, Digest: "sha256:modelsha"},
	})
	r := &GitHubModelResolver{
		Repo:       "k2-fsa/sherpa-onnx",
		AssetName:  wantName,
		HttpClient: srv.Client(),
	}
	got, err := r.ModelAsset(WithGitHubAPIRoot(t.Context(), srv.URL))
	if err != nil {
		t.Fatalf("ModelAsset: %v", err)
	}
	if got.Archive != "tar.bz2" {
		t.Fatalf("expected tar.bz2, got %s", got.Archive)
	}
	if got.SHA256 != "modelsha" {
		t.Fatalf("expected SHA from digest, got %q", got.SHA256)
	}
	if got.StripDir != wantName[:len(wantName)-len(".tar.bz2")] {
		t.Fatalf("StripDir should default to asset-name sans .tar.bz2: got %q", got.StripDir)
	}
}

func TestGitHubResolverRejectsEmptyRepo(t *testing.T) {
	r := &GitHubWorkerResolver{}
	_, err := r.WorkerAsset(t.Context())
	if err == nil || !strings.Contains(err.Error(), "empty repo") {
		t.Fatalf("expected empty-repo error, got %v", err)
	}
}
