package me

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeWorkflowFile writes a workflow file into a directory.
func writeWorkflowFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestCatalog_TheNameIsTheFileName — a workflow's identity is the base
// name of its file.
func TestCatalog_TheNameIsTheFileName(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowFile(t, dir, "fix.yml", "title: do the thing\njobs:\n  main:\n    steps: [{message: hi}]\n")

	c := NewCatalog(dir)
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	wf, ok := c.Lookup("fix")
	if !ok {
		t.Fatalf("the workflow was not found: %v", c.Names())
	}
	if wf.Title != "do the thing" {
		t.Errorf("Title = %q", wf.Title)
	}
	if wf.Name != "fix" {
		t.Errorf("Name = %q, want fix", wf.Name)
	}
}

// TestCatalog_ReadsBothExtensions — a workflow is a *.yaml or a *.yml
// file.
func TestCatalog_ReadsBothExtensions(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowFile(t, dir, "fix.yaml", "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeWorkflowFile(t, dir, "other.yml", "title: two\njobs:\n  main:\n    steps: [{message: hi}]\n")

	c := NewCatalog(dir)
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	names := c.Names()
	if len(names) != 2 || names[0] != "fix" || names[1] != "other" {
		t.Errorf("names = %v, want fix and other", names)
	}
}

// TestCatalog_AnUnchangedFileIsNotParsedAgain — the fingerprint says
// whether a file changed since it was parsed.
func TestCatalog_AnUnchangedFileIsNotParsedAgain(t *testing.T) {
	dir := t.TempDir()
	path := writeWorkflowFile(t, dir, "fix.yml",
		"title: first\njobs:\n  main:\n    steps: [{message: one}]\n")

	c := NewCatalog(dir)
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mod := info.ModTime()

	// The file changes, but its fingerprint does not: the catalogue
	// keeps the workflow it parsed.
	writeWorkflowFile(t, dir, "fix.yml",
		"title: other\njobs:\n  main:\n    steps: [{message: two}]\n")
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	wf, ok := c.Lookup("fix")
	if !ok {
		t.Fatal("the workflow is gone")
	}
	if wf.Title != "first" {
		t.Errorf("Title = %q, want the workflow the catalogue parsed", wf.Title)
	}

	// The fingerprint changes: the file is parsed again.
	later := mod.Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	wf, ok = c.Lookup("fix")
	if !ok {
		t.Fatal("the workflow is gone")
	}
	if wf.Title != "other" {
		t.Errorf("Title = %q, want the edited workflow", wf.Title)
	}
}

// TestCatalog_ANewFileIsPickedUpAndAGoneOneIsDropped — the catalogue
// holds what the directory holds.
func TestCatalog_ANewFileIsPickedUpAndAGoneOneIsDropped(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowFile(t, dir, "fix.yml", "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")

	c := NewCatalog(dir)
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := c.Lookup("fix"); !ok {
		t.Fatal("the workflow was not found")
	}

	writeWorkflowFile(t, dir, "other.yml", "title: two\njobs:\n  main:\n    steps: [{message: hi}]\n")
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := c.Lookup("other"); !ok {
		t.Fatalf("the new workflow was not found: %v", c.Names())
	}

	if err := os.Remove(filepath.Join(dir, "fix.yml")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := c.Lookup("fix"); ok {
		t.Error("the workflow of a file that is gone is still offered")
	}
}

// TestCatalog_ABrokenFileIsAFailureOfItsOwnName — the name a broken
// file would have provided reports why it is unusable.
func TestCatalog_ABrokenFileIsAFailureOfItsOwnName(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowFile(t, dir, "fix.yml", "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeWorkflowFile(t, dir, "broken.yml", "goal: no title\njobs:\n  main:\n    steps: [{message: hi}]\n")

	c := NewCatalog(dir)
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if names := c.Names(); len(names) != 1 || names[0] != "fix" {
		t.Errorf("names = %v, want fix", names)
	}
	failure, ok := c.Failure("broken")
	if !ok {
		t.Fatalf("the broken file carries no failure: %v", c.Failures())
	}
	if !strings.Contains(failure.Error(), "broken.yml") {
		t.Errorf("failure = %v, want it to name the file", failure)
	}
	if !strings.Contains(failure.Error(), "title") {
		t.Errorf("failure = %v, want it to name the replacement", failure)
	}
	if _, ok := c.Failure("fix"); ok {
		t.Error("a workflow that loaded carries a failure")
	}
}

// TestCatalog_TwoFilesWithTheSameNameMakeItUnusable — the name is
// ambiguous, and neither file is offered under it.
func TestCatalog_TwoFilesWithTheSameNameMakeItUnusable(t *testing.T) {
	dir := t.TempDir()
	writeWorkflowFile(t, dir, "fix.yaml", "title: one\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeWorkflowFile(t, dir, "fix.yml", "title: two\njobs:\n  main:\n    steps: [{message: hi}]\n")
	writeWorkflowFile(t, dir, "other.yml", "title: three\njobs:\n  main:\n    steps: [{message: hi}]\n")

	c := NewCatalog(dir)
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if names := c.Names(); len(names) != 1 || names[0] != "other" {
		t.Errorf("names = %v, want other", names)
	}
	failure, ok := c.Failure("fix")
	if !ok {
		t.Fatal("the ambiguous name carries no failure")
	}
	if !strings.Contains(failure.Error(), "duplicate workflow name") {
		t.Errorf("failure = %v, want the duplicate", failure)
	}
}

// TestCatalog_AMissingDirectoryIsNotAnError — a daemon without
// workflows is simply idle.
func TestCatalog_AMissingDirectoryIsNotAnError(t *testing.T) {
	c := NewCatalog(filepath.Join(t.TempDir(), "nowhere"))
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if names := c.Names(); len(names) != 0 {
		t.Errorf("names = %v, want none", names)
	}
	if failures := c.Failures(); len(failures) != 0 {
		t.Errorf("failures = %v, want none", failures)
	}
}
