package stt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func sha256OfBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// buildTarGz returns a tar.gz archive containing the named
// files.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// fakeWorkerResolver returns a WorkerResolver that emits
// the supplied asset. Tests use it to drive the installer
// without a GitHub dependency.
type fakeWorkerResolver struct {
	asset Asset
	err   error
	calls atomic.Int32
}

func (f *fakeWorkerResolver) WorkerAsset(ctx context.Context) (Asset, error) {
	f.calls.Add(1)
	return f.asset, f.err
}

type fakeModelResolver struct {
	asset Asset
	err   error
	calls atomic.Int32
}

func (f *fakeModelResolver) ModelAsset(ctx context.Context) (Asset, error) {
	f.calls.Add(1)
	return f.asset, f.err
}

// newTLSServer starts an httptest server with a self-signed
// cert and returns its URL plus a *http.Client whose TLS
// config trusts only that cert.
func newTLSServer(t *testing.T, h http.Handler) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

func TestInstallRejectsNonHTTPS(t *testing.T) {
	dir := t.TempDir()
	// A non-HTTPS asset URL makes downloadTo fail with
	// the refusing-non-HTTPS error before any disk write.
	wr := &fakeWorkerResolver{asset: Asset{
		URL:     "http://insecure.example/w",
		Archive: "tar.gz",
		Binary:  "nightme-stt",
		SHA256:  strings.Repeat("0", 64),
		Size:    1,
		Tag:     "v0.0.0",
	}}
	mr := &fakeModelResolver{asset: Asset{
		URL:      "http://insecure.example/m",
		Archive:  "tar.bz2",
		StripDir: "irrelevant",
		SHA256:   strings.Repeat("0", 64),
		Size:     1,
		Tag:      "v0.0.0",
	}}
	inst, err := NewInstaller(dir, wr, mr)
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	_, err = inst.Install(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "non-HTTPS") {
		t.Fatalf("expected non-HTTPS rejection, got %v", err)
	}
}

func TestInstallDownloadsWorkerVerifiesSHA(t *testing.T) {
	workerBin := []byte("#!/bin/sh\necho nightme-stt stub\n")
	tarGz := buildTarGz(t, map[string]string{
		"nightme-stt": string(workerBin),
		"LICENSE":     "license",
		"README.md":   "readme",
	})
	sha := sha256OfBytes(tarGz)

	srvURL, client := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tarGz)))
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: Asset{
		URL:     srvURL + "/worker.tar.gz",
		SHA256:  sha,
		Size:    int64(len(tarGz)),
		Archive: "tar.gz",
		Binary:  "nightme-stt",
		Tag:     "v0.0.0-test",
	}}
	// Point the model at the same server but with a
	// wrong SHA — install should fail at the model phase
	// AFTER the worker has been activated.
	mr := &fakeModelResolver{asset: Asset{
		URL:      srvURL + "/model",
		SHA256:   strings.Repeat("d", 64),
		Size:     1,
		Archive:  "tar.bz2",
		StripDir: "irrelevant",
		Tag:      "v0.0.0-test",
	}}
	inst, err := NewInstaller(dir, wr, mr)
	if err != nil {
		t.Fatalf("NewInstaller: %v", err)
	}
	inst.http = client

	_, err = inst.Install(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected model-side failure, got nil")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected model error, got %v", err)
	}

	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	info, statErr := os.Stat(binPath)
	if statErr != nil {
		t.Fatalf("worker binary not installed: %v", statErr)
	}
	if info.Size() != int64(len(workerBin)) {
		t.Fatalf("worker size mismatch: got %d want %d", info.Size(), len(workerBin))
	}
	if info.Mode()&0o100 == 0 {
		t.Fatalf("worker not executable: mode=%v", info.Mode())
	}
}

func TestInstallRejectsSHA256Mismatch(t *testing.T) {
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": "x"})
	srvURL, client := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: Asset{
		URL:     srvURL + "/w",
		SHA256:  "deadbeef",
		Size:    int64(len(tarGz)),
		Archive: "tar.gz",
		Binary:  "nightme-stt",
		Tag:     "v0.0.0-test",
	}}
	mr := &fakeModelResolver{asset: Asset{
		URL:      srvURL + "/m",
		SHA256:   strings.Repeat("0", 64),
		Size:     1,
		Archive:  "tar.bz2",
		StripDir: "irrelevant",
		Tag:      "v0.0.0-test",
	}}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = client

	_, err := inst.Install(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected sha256 mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stt", "bin", "nightme-stt")); err == nil {
		t.Fatalf("worker binary present despite SHA mismatch — install leaked partial state")
	}
}

func TestInstallIdempotentWhenAlreadyPresent(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	workerSHA := sha256OfBytes(workerBin)

	dir := t.TempDir()

	// Pre-populate BOTH the worker binary AND the model
	// files so the SHA-skip / isModelInstalled checks
	// both fire — Install must not hit the network at all.
	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(binPath, workerBin, 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	info1, _ := os.Stat(binPath)

	modelDir := filepath.Join(dir, "stt", "models", "sensevoice")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir model: %v", err)
	}
	for name, body := range map[string]string{"model.int8.onnx": "fake-model-bytes", "tokens.txt": "fake-tokens"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	wr := &fakeWorkerResolver{asset: Asset{
		URL:     "https://example.invalid/never",
		SHA256:  workerSHA,
		Size:    int64(len(workerBin)),
		Archive: "tar.gz",
		Binary:  "nightme-stt",
		Tag:     "v0.0.0-test",
	}}
	mr := &fakeModelResolver{asset: Asset{
		URL:      "https://example.invalid/never",
		SHA256:   "x",
		Size:     1,
		Archive:  "tar.bz2",
		StripDir: "irrelevant",
		Tag:      "v0.0.0-test",
	}}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	res, err := inst.Install(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected no-op install to succeed, got %v", err)
	}
	if res.WorkerInstalled {
		t.Fatalf("worker marked installed; SHA-match skip should have fired")
	}
	if res.ModelInstalled {
		t.Fatalf("model marked installed; isModelInstalled skip should have fired")
	}
	if res.BytesDownloaded != 0 {
		t.Fatalf("expected 0 bytes downloaded, got %d", res.BytesDownloaded)
	}
	info2, _ := os.Stat(binPath)
	if info1.ModTime() != info2.ModTime() {
		t.Fatalf("worker binary mtime changed despite SHA-match skip")
	}
}

func TestResolverEmitsExpectedArchiveForCurrentPlatform(t *testing.T) {
	// Sanity: workerAssetSuffix picks the right suffix
	// per platform. This guards the GitHubWorkerResolver's
	// mapping against accidental regressions when a new OS
	// is added to the matrix.
	if runtime.GOOS == "windows" {
		got := workerAssetSuffix("windows", "amd64")
		if got != "_windows_amd64.zip" {
			t.Fatalf("windows suffix wrong: %q", got)
		}
	} else {
		got := workerAssetSuffix("linux", "amd64")
		if got != "_linux_amd64.tar.gz" {
			t.Fatalf("linux suffix wrong: %q", got)
		}
	}
}

func TestParseDigestField(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"sha256:abc123", "abc123"},
		{"SHA256:AbC123", "abc123"}, // lowercased
		{"", ""},
		{"md5:abc", ""},
	}
	for _, tc := range cases {
		if got := parseDigestField(tc.in); got != tc.want {
			t.Fatalf("parseDigestField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
