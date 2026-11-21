package main

import (
	"strings"
	"testing"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/version"
)

// requiredWorkflow is a workflow that needs a nightme this build may
// not be.
const requiredWorkflow = `title: review the changes
requires:
  nightme: ">=0.10.0"
jobs:
  main:
    steps: [{id: s1, message: /review}]
`

// buildVersion sets the version of the build for the test.
func buildVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = orig })
}

// TestWorkflowList_CarriesTheRequirement — the list says what a
// workflow needs and how this build stands against it.
func TestWorkflowList_CarriesTheRequirement(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "reviewer", requiredWorkflow)
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)
	buildVersion(t, "0.9.1")

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	for _, want := range []string{"nightme >= 0.10.0", "not satisfied"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
}

// TestWorkflowShow_CarriesTheRequirement — the detail view says what
// the workflow needs.
func TestWorkflowShow_CarriesTheRequirement(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "reviewer", requiredWorkflow)
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)
	buildVersion(t, "0.9.1")

	out := captureStdout(t, func() error {
		return runRoot("workflow", "show", "reviewer")
	})
	for _, want := range []string{"requires:", "nightme >= 0.10.0", "not satisfied"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
}

// TestWorkflowRun_CarriesTheRequirement — the dry run says what the
// workflow needs.
func TestWorkflowRun_CarriesTheRequirement(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "reviewer", requiredWorkflow)
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)
	buildVersion(t, "0.9.1")

	out := captureStdout(t, func() error {
		return runRoot("workflow", "run", "reviewer")
	})
	if !strings.Contains(out, "nightme >= 0.10.0") {
		t.Errorf("output = %q, want the requirement", out)
	}
}

// TestWorkflowList_ADevelopmentBuildDoesNotCheckTheRequirement — a
// build that carries no release version says the requirement is not
// checked.
func TestWorkflowList_ADevelopmentBuildDoesNotCheckTheRequirement(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, "reviewer", requiredWorkflow)
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)
	buildVersion(t, "dev")

	out := captureStdout(t, func() error {
		return runRoot("workflow", "list")
	})
	for _, want := range []string{"nightme >= 0.10.0", "not checked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "not satisfied") {
		t.Errorf("output = %q, want no verdict on an unchecked build", out)
	}
}

// TestWorkflowValidate_WarnsAboutAnUnsatisfiedRequirement — the
// requirement is a warning: it goes to stderr and the exit code does
// not change.
func TestWorkflowValidate_WarnsAboutAnUnsatisfiedRequirement(t *testing.T) {
	dir := t.TempDir()
	path := writeBundle(t, dir, "reviewer", requiredWorkflow)
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)
	buildVersion(t, "0.9.1")

	var err error
	out := captureStdout(t, func() error {
		err = runRoot("workflow", "validate", path)
		return nil
	})
	if out != "" {
		t.Errorf("output = %q, want nothing", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeSuccess {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeSuccess)
	}
	errOut := captureStderr(t, func() error {
		return runRoot("workflow", "validate", path)
	})
	if !strings.Contains(errOut, "this workflow needs nightme >= 0.10.0") {
		t.Errorf("stderr = %q, want the warning", errOut)
	}
	if !strings.Contains(errOut, path) {
		t.Errorf("stderr = %q, want it to name the file", errOut)
	}
}

// TestWorkflowValidate_ADevelopmentBuildWarnsAboutNothing — a build
// that carries no release version does not check the requirement.
func TestWorkflowValidate_ADevelopmentBuildWarnsAboutNothing(t *testing.T) {
	dir := t.TempDir()
	path := writeBundle(t, dir, "reviewer", requiredWorkflow)
	t.Setenv("NIGHTME_WORKFLOWS_DIR", dir)
	buildVersion(t, "dev")

	var err error
	errOut := captureStderr(t, func() error {
		err = runRoot("workflow", "validate", path)
		return nil
	})
	if errOut != "" {
		t.Errorf("stderr = %q, want nothing", errOut)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeSuccess {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeSuccess)
	}
}
