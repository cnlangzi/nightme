// Package wf implements the `/wf` slash command.
//
//	/wf install [-f] <owner>/<name>
//	/wf uninstall <owner>/<name>
//
// The command fetches a published workflow from the hub and installs
// it into the daemon's workflows directory, where the personal agent
// finds it under the name of its address. An address that is already
// installed is left alone unless the install is forced; the uninstall
// takes an installed workflow away again.
//
// The install is reported in the chat as a three-row checklist — the
// download, the validation and the write — and the uninstall as a
// one-row checklist. The command posts no reply text of its own: the
// checklist is the verdict.
package wf

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/cnlangzi/nightme/pkg/wfe"
)

// The hub: the published workflows live in this repository, one bundle
// per workflow, under workflows/<owner>/<name>/workflow.yml.
const (
	hubRepo   = "nightme-dev/workflows"
	hubBranch = "main"
	hubPath   = "workflows/%s/%s/workflow.yml"
)

// workflowFileName is the name the hub publishes a workflow under. It
// is the name the install writes it under too: the workflow file of
// the bundle.
const workflowFileName = "workflow.yml"

// Address is the address of a published workflow: <owner>/<name>.
type Address struct {
	// Owner is the namespace the workflow is published under.
	Owner string
	// Name is the workflow's short name.
	Name string
}

// String renders the address the way it is written: <owner>/<name>.
func (a Address) String() string {
	return a.Owner + "/" + a.Name
}

// ownerRe is the shape of a namespace: lowercase letters, digits,
// dashes and underscores, starting with a letter or a digit.
//
// The owner becomes a directory of the workflows directory and a
// segment of the hub's URL, so it has to stay one safe path segment:
// no dots, no separators, and no case to collide on a case-insensitive
// file system.
var ownerRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62})$`)

// nameRe is the shape of a workflow's short name: lowercase
// kebab-case.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ParseAddress parses "<owner>/<name>" and checks its shape.
//
// The shape rules are the hub's: the owner is the namespace the
// workflow is published under and the name is a lowercase kebab-case
// short name. An address that does not fit them is refused before
// anything is fetched, so an address can never reach outside the
// hub's tree.
func ParseAddress(s string) (Address, error) {
	trimmed := strings.TrimSpace(s)
	owner, name, ok := strings.Cut(trimmed, "/")
	if !ok || owner == "" || name == "" {
		return Address{}, fmt.Errorf("the address %q is not <owner>/<name>", s)
	}
	if strings.Contains(name, "/") {
		return Address{}, fmt.Errorf("the address %q has more than two parts", s)
	}
	if !ownerRe.MatchString(owner) {
		return Address{}, fmt.Errorf("the owner %q is not a lowercase namespace name", owner)
	}
	if !nameRe.MatchString(name) {
		return Address{}, fmt.Errorf("the workflow name %q is not a lowercase kebab-case name", name)
	}
	if wfe.IsReservedDir(owner) {
		// The owner becomes a directory under the workflows directory,
		// and two of those names are reserved: the workflows under them
		// are never scanned, so an install there would be invisible.
		return Address{}, fmt.Errorf("the owner %q is reserved by the workflows directory", owner)
	}
	return Address{Owner: owner, Name: name}, nil
}

// Source is one place a workflow file is fetched from.
type Source struct {
	// Name is the tier's name: it is what the checklist says.
	Name string
	// BaseURL is the prefix the file's URL is built from.
	BaseURL string
}

// URL returns the URL of the workflow file of addr at this tier.
func (s Source) URL(addr Address) string {
	return s.BaseURL + fmt.Sprintf(hubPath, addr.Owner, addr.Name)
}

// Sources returns the tiers a workflow file is fetched from, in the
// order they are tried.
//
// GitHub comes first; the two CDNs are the fallbacks for the places
// where GitHub is out of reach.
func Sources() []Source {
	return []Source{
		{
			Name:    "github",
			BaseURL: "https://raw.githubusercontent.com/" + hubRepo + "/" + hubBranch + "/",
		},
		{
			Name:    "jsdelivr",
			BaseURL: "https://cdn.jsdelivr.net/gh/" + hubRepo + "@" + hubBranch + "/",
		},
		{
			Name:    "jsdmirror",
			BaseURL: "https://cdn.jsdmirror.com/gh/" + hubRepo + "@" + hubBranch + "/",
		},
	}
}
