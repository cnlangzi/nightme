// Package wfname resolves the name of a workflow.
//
// A workflow of the project's own namespace is addressed by its short
// name: "coder" is the workflow "nightme/coder". Every place that
// takes a workflow name resolves it through this package, so the
// short name is written the same way everywhere.
package wfname

import "strings"

// Owner is the namespace the project's own workflows live under.
const Owner = "nightme"

// Expand turns a name that carries no owner into the name it stands
// for: "coder" is "nightme/coder". A name that carries an owner is
// returned as it is.
//
// The expansion is a change of spelling only: whether the workflow
// exists — in the workflows directory or on the hub — is the caller's
// business.
func Expand(s string) string {
	if s == "" || hasOwner(s) {
		return s
	}
	return Owner + "/" + s
}

// Names returns the names a workflow name answers to, most specific
// first: "coder" answers to the workflow "coder" and to the workflow
// "nightme/coder".
//
// A workflow's name is the path of its bundle under the workflows
// directory, so a name that carries no owner is a workflow of its
// own: it comes first, and the project's own namespace is the
// fallback.
func Names(s string) []string {
	if s == "" || hasOwner(s) {
		return []string{s}
	}
	return []string{s, Owner + "/" + s}
}

// hasOwner reports whether the name carries an owner: a slash in it.
func hasOwner(s string) bool {
	return strings.Contains(s, "/")
}
