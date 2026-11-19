// Package wfname resolves the name of a workflow.
//
// A workflow of the project's own namespace is addressed by its short
// name: "@coder" is the workflow "nightme/coder". Every place that
// takes a workflow name resolves it through this package, so the
// shorthand is written the same way everywhere.
package wfname

import (
	"errors"
	"strings"
)

// Owner is the namespace the project's own workflows live under.
const Owner = "nightme"

// marker is what a shorthand starts with.
const marker = "@"

// Expand turns "@<name>" into the workflow name it stands for:
// "<Owner>/<name>". A name that is not the shorthand is returned as
// it is.
//
// The expansion is a change of spelling only: whether the workflow
// exists — in the workflows directory or on the hub — is the caller's
// business.
func Expand(s string) (string, error) {
	if !strings.HasPrefix(s, marker) {
		return s, nil
	}
	name := strings.TrimPrefix(s, marker)
	if name == "" {
		return "", errors.New("the shorthand needs a workflow name")
	}
	if strings.Contains(name, "/") {
		return "", errors.New("the shorthand takes a workflow name only")
	}
	return Owner + "/" + name, nil
}
