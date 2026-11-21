package me

import (
	"github.com/cnlangzi/nightme/internal/version"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// requirementError returns the error of a workflow this build cannot
// run: the workflow needs a nightme this build is not.
//
// A build that carries no release version is not held to the
// requirement. A development build is the newest code there is, and
// the CI build carries no release at all — neither of them may be
// stopped by a version number that names no release.
func requirementError(wf *wfe.Workflow) error {
	if wf == nil || !version.IsRelease() {
		return nil
	}
	return wf.Requires.Check(version.Version)
}
