// catalog.go — the daemon's view of the workflows directory.
//
// The directory is read when a workflow is asked for, not when the
// daemon starts: an edited file takes effect on the next `/me start`,
// and nothing watches the directory in between.
//
// Every file is remembered by its fingerprint — its size and its
// modification time — so a refresh parses only what changed. Every
// base name carries either the workflow the file holds or the failure
// that made the name unusable.
package me

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/wfe"
)

// catalogEntry is one workflow file of the directory.
type catalogEntry struct {
	// path is the file the entry came from.
	path string
	// size and mod are the file's fingerprint: the pair says whether
	// the file changed since it was parsed.
	size int64
	mod  time.Time
	// wf is the parsed workflow, when the file loaded.
	wf *wfe.Workflow
	// err is why the file did not load.
	err error
}

// name returns the base name the entry provides.
func (e *catalogEntry) name() string {
	if e.wf != nil {
		return e.wf.Name
	}
	return wfe.BaseName(e.path)
}

// Catalog is the daemon's view of the workflows directory.
//
// A workflow's identity is the base name of its file: `fix.yml` is the
// workflow `fix`, and two files that share a base name make that name
// unusable.
type Catalog struct {
	dir string

	mu sync.Mutex
	// files holds every file of the directory, by path.
	files map[string]*catalogEntry
	// byName holds the workflows that loaded, by base name.
	byName map[string]*catalogEntry
	// errs holds the failures, by base name.
	errs map[string]error
}

// NewCatalog returns a catalogue over the workflows directory.
//
// The directory is not read yet: the first Refresh reads it.
func NewCatalog(dir string) *Catalog {
	return &Catalog{
		dir:    dir,
		files:  map[string]*catalogEntry{},
		byName: map[string]*catalogEntry{},
		errs:   map[string]error{},
	}
}

// Refresh re-reads the workflows directory.
//
// Every file is stat'ed: a file whose fingerprint did not change keeps
// its workflow, a file that is new or changed is parsed again, and a
// file that is gone is dropped. The base names are checked afterwards:
// a name two files claim is a failure, and neither file is offered
// under it.
//
// A file that does not load is remembered under its base name, so the
// name it would have provided reports the reason instead of "there is
// no workflow named".
//
// A directory that cannot be read is an error, and the catalogue keeps
// what it has: the workflows it holds are still the ones on disk.
func (c *Catalog) Refresh() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	paths, err := wfe.WorkflowPaths(c.dir)
	if err != nil {
		return fmt.Errorf("the workflows directory %s cannot be read: %w", c.dir, err)
	}
	files := make(map[string]*catalogEntry, len(paths))
	for _, path := range paths {
		files[path] = c.load(path)
	}
	c.files = files
	c.index()
	return nil
}

// load returns the entry of a file: the workflow it holds, or the
// failure that stopped it.
//
// A file whose fingerprint did not change keeps the entry it has.
func (c *Catalog) load(path string) *catalogEntry {
	info, err := os.Stat(path)
	if err != nil {
		return &catalogEntry{path: path, err: fmt.Errorf("%s could not be read: %w", filepath.Base(path), err)}
	}
	if prev, ok := c.files[path]; ok && prev.size == info.Size() && prev.mod.Equal(info.ModTime()) {
		return prev
	}
	entry := &catalogEntry{path: path, size: info.Size(), mod: info.ModTime()}
	wf, err := wfe.LoadFile(path)
	if err != nil {
		entry.err = err
		return entry
	}
	entry.wf = wf
	return entry
}

// index sorts the files into the workflows they provide and the
// failures they carry.
//
// The caller holds the lock.
func (c *Catalog) index() {
	claims := map[string][]*catalogEntry{} // base name -> the files that claim it
	for _, path := range c.sortedPaths() {
		entry := c.files[path]
		name := entry.name()
		if name == "" {
			continue
		}
		claims[name] = append(claims[name], entry)
	}
	byName := make(map[string]*catalogEntry, len(claims))
	errs := map[string]error{}
	for _, name := range sortedNames(claims) {
		files := claims[name]
		if len(files) > 1 {
			errs[name] = fmt.Errorf("wfe: duplicate workflow name %q in %s and %s",
				name, files[0].path, files[1].path)
			continue
		}
		entry := files[0]
		if entry.err != nil {
			errs[name] = fmt.Errorf("%s could not be loaded: %w", filepath.Base(entry.path), entry.err)
			continue
		}
		byName[name] = entry
	}
	c.byName = byName
	c.errs = errs
}

// sortedPaths returns the paths of the directory, sorted.
func (c *Catalog) sortedPaths() []string {
	out := make([]string, 0, len(c.files))
	for path := range c.files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// Lookup returns the workflow with the given base name.
//
// The name is matched as it is spelled on disk.
func (c *Catalog) Lookup(name string) (*wfe.Workflow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byName[name]
	if !ok {
		return nil, false
	}
	return entry.wf, true
}

// Names returns the base names of the workflows that loaded, sorted.
func (c *Catalog) Names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sortedNames(c.byName)
}

// Failure returns why the given base name is unusable.
//
// A file that did not load carries its failure under the name it would
// have provided; a name two files claim carries the duplicate.
func (c *Catalog) Failure(name string) (error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	err, ok := c.errs[name]
	return err, ok
}

// Failures returns the failures of the directory, sorted by name.
func (c *Catalog) Failures() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]error, 0, len(c.errs))
	for _, name := range sortedNames(c.errs) {
		out = append(out, c.errs[name])
	}
	return out
}

// sortedNames returns the keys of a map, sorted.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
