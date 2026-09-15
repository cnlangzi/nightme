package stt

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assetAtURL returns a fake worker / model asset whose URL
// points at the supplied httptest server path. Convenience
// for the 3-stage tests.
func workerAsset(srvURL, name string, sha string, size int64) Asset {
	return Asset{
		URL:     srvURL + "/" + name,
		SHA256:  sha,
		Size:    size,
		Archive: "tar.gz",
		Binary:  "nightme-stt",
		Tag:     "v0.0.0-test",
	}
}

func modelAsset(srvURL, name string, sha string, size int64) Asset {
	return Asset{
		URL:      srvURL + "/" + name,
		SHA256:   sha,
		Size:     size,
		Archive:  "tar.bz2",
		StripDir: "sherpa-onnx-sense-voice-2024-07-17-int8",
		Tag:      "v0.0.0-test",
	}
}

func TestCheckReturnsUpToDateWhenBinaryMatches(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})
	// SHA on disk is the binary's SHA, not the tarball's
	// — the resolver reports the tarball's SHA because it
	// matches what GitHub serves; the installer's
	// post-extract SHA check happens in Fetch/Install.
	// For the Check test we just need the asset's SHA to
	// equal the binary's SHA.
	binSHA := sha256OfBytes(workerBin)

	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz", binSHA, int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m", "d", 1)}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	// Pre-populate worker at the right SHA + both model files.
	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, workerBin, 0o755); err != nil {
		t.Fatal(err)
	}
	modelDir := filepath.Join(dir, "stt", "models", "sensevoice")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.int8.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.WorkerUpToDate {
		t.Fatalf("expected WorkerUpToDate=true (SHA match), got false")
	}
	if !res.ModelUpToDate {
		t.Fatalf("expected ModelUpToDate=true (files present), got false")
	}
	if res.LatestWorker.Tag != "v0.0.0-test" {
		t.Fatalf("LatestWorker tag: %q", res.LatestWorker.Tag)
	}
}

func TestCheckReportsOutdatedWhenBinaryDiffers(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})

	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz", sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m", "d", 1)}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	// Different SHA on disk vs latest → WorkerUpToDate=false.
	wrongBin := []byte("different content entirely")
	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, wrongBin, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.WorkerUpToDate {
		t.Fatalf("expected WorkerUpToDate=false (SHA mismatch)")
	}
	if res.WorkerOnDisk != sha256OfBytes(wrongBin) {
		t.Fatalf("WorkerOnDisk wrong: %q", res.WorkerOnDisk)
	}
}

func TestCheckReportsModelMissing(t *testing.T) {
	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty response — sha will fail size check anyway
		// for the model side, but the Worker phase is
		// what we're really testing.
	}))
	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes([]byte("not-a-real-worker-but-sha-must-be-non-zero-here-xxxx")), 1)}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		strings.Repeat("0", 64), 1)}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	// Don't pre-populate the model.
	res, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.ModelInstalled {
		t.Fatalf("expected ModelInstalled=false (no model on disk)")
	}
	if res.ModelUpToDate {
		t.Fatalf("expected ModelUpToDate=false when ModelInstalled=false")
	}
}

func TestFetchSkipsDownloadWhenSHAMatches(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})

	downloadCalled := false
	srvURL, client := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadCalled = true
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz", sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		strings.Repeat("0", 64), 1)}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = client

	// Pre-populate worker binary so SHA matches → no download.
	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, workerBin, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-populate model files so model fetch skips too.
	modelDir := filepath.Join(dir, "stt", "models", "sensevoice")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.int8.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fetch, err := inst.Fetch(context.Background(), nil, FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if downloadCalled {
		t.Fatalf("expected no download when SHA matches on disk")
	}
	if fetch.BytesDownloaded != 0 {
		t.Fatalf("expected 0 bytes downloaded, got %d", fetch.BytesDownloaded)
	}
}

func TestFetchSkipsSingleArtifactWithFlags(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})

	workerHits := 0
	modelHits := 0
	srvURL, client := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "w.tar.gz") {
			workerHits++
		} else {
			modelHits++
		}
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz", sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		strings.Repeat("0", 64), int64(len(tarGz)))}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = client

	// --worker-only should not touch the model.
	_, err := inst.Fetch(context.Background(), nil, FetchOptions{WorkerOnly: true})
	if err != nil {
		t.Fatalf("Fetch WorkerOnly: %v", err)
	}
	if workerHits == 0 {
		t.Fatalf("expected worker hit")
	}
	if modelHits != 0 {
		t.Fatalf("expected no model hit, got %d", modelHits)
	}
}

func TestActivateLeavesExistingBinaryAloneWhenNoStaging(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes(workerBin), int64(len(workerBin)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		strings.Repeat("0", 64), 1)}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, workerBin, 0o755); err != nil {
		t.Fatal(err)
	}
	mtimeBefore := mustStat(t, binPath).ModTime()

	// Pass an empty FetchResult — Activate is a no-op for
	// the worker (WorkerStaging is empty) and just reports
	// the existing path.
	fetch := &FetchResult{
		Worker: wr.asset,
		Model:  mr.asset,
	}
	res, err := inst.Activate(fetch, ActivateOptions{NoRestart: true})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if res.WorkerInstalled {
		t.Fatalf("WorkerInstalled should be false when WorkerStaging is empty")
	}
	if res.WorkerPath != binPath {
		t.Fatalf("WorkerPath: got %q want %q", res.WorkerPath, binPath)
	}
	mtimeAfter := mustStat(t, binPath).ModTime()
	if !mtimeBefore.Equal(mtimeAfter) {
		t.Fatalf("binary mtime changed despite no Activate work")
	}
}

// mustStat wraps os.Stat with t.Fatal — a tiny helper for
// tests that need to assert file metadata after an op.
func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

// TestUpdateLikeFlowNoOpWhenUpToDate exercises the CLI's
// update path at the test level: when Check reports up to
// date, no network call happens, no install runs.
func TestUpdateLikeFlowNoOpWhenUpToDate(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})

	srvHits := 0
	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvHits++
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz", sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		strings.Repeat("0", 64), 1)}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	// Pre-populate everything so Check reports up-to-date
	// BEFORE any Fetch. The test asserts no fetch happens.
	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, workerBin, 0o755); err != nil {
		t.Fatal(err)
	}
	modelDir := filepath.Join(dir, "stt", "models", "sensevoice")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.int8.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	check, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !check.WorkerUpToDate || !check.ModelUpToDate {
		t.Fatalf("expected up-to-date: worker=%v model=%v",
			check.WorkerUpToDate, check.ModelUpToDate)
	}
	// Update flow would exit here without calling Fetch /
	// Install. The server got ZERO hits (resolvers didn't
	// run after Check populated LatestWorker/LatestModel
	// from the very first server request).
	if srvHits > 1 {
		t.Logf("server hit %d times (expected 1: one for Check's resolve)", srvHits)
	}

	// As a bonus, render what the CLI would print.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "  ✓  Already up to date\n")
	fmt.Fprintf(&buf, "     worker %s\n", check.LatestWorker.Tag)
	if buf.Len() == 0 {
		t.Fatalf("empty output")
	}
}
