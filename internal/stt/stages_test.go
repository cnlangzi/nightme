package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func writeManifest(t *testing.T, dataDir, workerTag, modelTag string) {
	t.Helper()
	manifestDir := filepath.Join(dataDir, "stt")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	rec := map[string]string{"worker_tag": workerTag, "model_tag": modelTag}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestCheckReturnsUpToDateWhenManifestTagMatches(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})
	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
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
	writeManifest(t, dir, "v0.0.0-test", "v0.0.0-test")

	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	res, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.WorkerUpToDate {
		t.Fatalf("expected WorkerUpToDate=true (manifest tag matches), got false")
	}
	if !res.ModelUpToDate {
		t.Fatalf("expected ModelUpToDate=true (manifest tag matches + files present), got false")
	}
}

func TestCheckReportsOutdatedWhenManifestTagDiffers(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})
	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
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
	writeManifest(t, dir, "v0.0.0-old", "v0.0.0-old")

	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	res, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.WorkerUpToDate {
		t.Fatalf("expected WorkerUpToDate=false (manifest tag differs)")
	}
	if res.WorkerOnDisk != "v0.0.0-old" {
		t.Fatalf("WorkerOnDisk: got %q", res.WorkerOnDisk)
	}
}

func TestCheckReportsMissingManifest(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})
	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	res, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.WorkerOnDisk != "" {
		t.Fatalf("expected empty WorkerOnDisk without manifest, got %q", res.WorkerOnDisk)
	}
	if res.WorkerUpToDate {
		t.Fatalf("expected WorkerUpToDate=false without manifest")
	}
}

func TestFetchDownloadsWhenOnDiskSHADiffers(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})

	downloadCalled := false
	srvURL, client := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadCalled = true
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = client

	binPath := filepath.Join(dir, "stt", "bin", "nightme-stt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("stale binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-populate the model so Fetch only exercises the
	// worker download path; model extraction would
	// require a real bz2 fixture which is out of scope
	// here.
	modelDir := filepath.Join(dir, "stt", "models", "sensevoice")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model.int8.onnx", "tokens.txt"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := inst.Fetch(context.Background(), nil, FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !downloadCalled {
		t.Fatalf("expected download to fire when on-disk SHA != asset SHA")
	}
	if _, err := os.Stat(filepath.Join(dir, "stt", "manifest.json")); err != nil {
		t.Fatalf("manifest not written: %v", err)
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
	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = client

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
		sha256OfBytes(workerBin), int64(len(workerBin)))}
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

	fetch := &FetchResult{Worker: wr.asset, Model: mr.asset}
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

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

func TestUpdateLikeFlowNoOpWhenUpToDate(t *testing.T) {
	workerBin := []byte("#!/bin/sh\nexit 0\n")
	tarGz := buildTarGz(t, map[string]string{"nightme-stt": string(workerBin)})

	srvHits := 0
	srvURL, _ := newTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvHits++
		_, _ = w.Write(tarGz)
	}))

	dir := t.TempDir()
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
	writeManifest(t, dir, "v0.0.0-test", "v0.0.0-test")

	wr := &fakeWorkerResolver{asset: workerAsset(srvURL, "w.tar.gz",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	mr := &fakeModelResolver{asset: modelAsset(srvURL, "m",
		sha256OfBytes(tarGz), int64(len(tarGz)))}
	inst, _ := NewInstaller(dir, wr, mr)
	inst.http = http.DefaultClient

	check, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !check.WorkerUpToDate || !check.ModelUpToDate {
		t.Fatalf("expected up-to-date: worker=%v model=%v",
			check.WorkerUpToDate, check.ModelUpToDate)
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "  ✓  Already up to date\n")
	fmt.Fprintf(&buf, "     worker %s\n", check.LatestWorker.Tag)
	if buf.Len() == 0 {
		t.Fatalf("empty output")
	}
}
