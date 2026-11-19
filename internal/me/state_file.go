// state_file.go — the run's state on the disk.
//
// The state of a run is the memory of that run: a daemon that comes
// back reads it and takes the run up where it was. The write of the
// state has to survive whatever else touches the file — a reader that
// holds it open, another write that happens at the same time — or the
// run comes back without its place.

package me

import (
	"os"
	"path/filepath"
	"time"
)

// stateReplaceAttempts is how often the state file is replaced before
// the daemon gives up on it.
//
// A file that a reader holds open cannot be replaced on Windows: the
// rename is refused until the reader lets go. The state of a run is
// worth a few tries.
const stateReplaceAttempts = 5

// stateReplaceBackoff is how long the daemon waits before the next
// try. The wait grows with every try.
const stateReplaceBackoff = 5 * time.Millisecond

// writeStateTemp writes the state to a temp file next to path and
// returns the name of that file.
//
// The temp file has a name of its own: two writes that happen at the
// same time do not write into the same file, and the state that
// reaches the disk is the state one of them wrote whole.
func writeStateTemp(path string, raw []byte) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// replaceStateFile renames the temp file over the state file.
//
// A reader that holds the state file open keeps the rename from
// happening: Windows refuses it until the reader lets go. The state of
// a run is the memory of that run — a daemon that comes back without
// it loses the run's place — so the rename is tried again while the
// error is the one a reader causes.
func replaceStateFile(src, dst string) error {
	return replaceFileRetrying(src, dst, os.Rename, transientReplaceError)
}

// replaceFileRetrying renames src over dst and tries again while the
// error is a transient one.
//
// The wait grows with every try: a reader that holds the file open for
// a moment is gone by the second try, and one that holds it longer
// does not hold the daemon forever.
func replaceFileRetrying(src, dst string, rename func(string, string) error, transient func(error) bool) error {
	var err error
	for attempt := 0; attempt < stateReplaceAttempts; attempt++ {
		err = rename(src, dst)
		if err == nil || !transient(err) {
			return err
		}
		time.Sleep(stateReplaceBackoff * time.Duration(attempt+1))
	}
	return err
}
