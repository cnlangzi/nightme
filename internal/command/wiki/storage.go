package wiki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrWikiAlreadyExists is returned by Scaffold when the
// target directory or metadata file already exists. /wiki
// init never overwrites user content — the user must inspect
// and remove the conflict before retrying.
var ErrWikiAlreadyExists = errors.New("wiki already exists")

// Scaffold creates the wiki skeleton at cwd/wiki plus
// cwd/wiki.yml. Per-package skeleton pages are emitted under
// wiki/modules/ for every Go package discovered in cwd; the
// module list is mirrored into wiki.yml.modules[] and
// llms.txt's "## Modules" section.
//
// All writes use atomic rename: each file is written to a
// temp sibling, then renamed into place, so a crash mid-init
// cannot leave half-written wiki files. Temp files left
// behind after a crash are not auto-cleaned.
//
// cwd must be an absolute path to an existing directory.
// The caller (runInit) is responsible for validating cwd
// via command.RequireActiveCwd.
//
// Returns:
//   - written: list of paths (relative to cwd) that were
//     created, in deterministic order: llms.txt,
//     architecture.md, glossary.md, [modules/<slug>.md ...,
//     or modules/.gitkeep when no modules detected],
//     wiki.yml.
//   - err: ErrWikiAlreadyExists if the target dir or
//     wiki.yml already exists with content; other errors
//     wrapped with file-system context.
func Scaffold(cwd string) (written []string, err error) {
	wikiDir := filepath.Join(cwd, "wiki")
	wikiYml := filepath.Join(cwd, "wiki.yml")

	if err := refuseIfPresent(wikiDir, wikiYml); err != nil {
		return nil, err
	}

	// Discover modules BEFORE creating wiki/ so the walker
	// (which skips "wiki" by name) does not have to deal with
	// the directory it is about to create. Discovery reads
	// only the source tree — it is cheap.
	modules, err := discoverModules(cwd)
	if err != nil {
		return nil, fmt.Errorf("discover modules: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(wikiDir, "modules"), 0o755); err != nil {
		return nil, fmt.Errorf("create wiki dir: %w", err)
	}

	// Core files first — their order is the user-visible
	// "created:" list, so keep it stable across runs.
	files := []struct {
		path    string
		content string
	}{
		{filepath.Join(wikiDir, "llms.txt"), llmsTxtSkeletonWithModules(projectName(cwd), modules)},
		{filepath.Join(wikiDir, "architecture.md"), architectureSkeleton()},
		{filepath.Join(wikiDir, "glossary.md"), glossarySkeleton()},
	}

	// Per-module skeleton pages, alphabetically by path
	// (discoverModules already sorts). Append AFTER the core
	// three so the user-facing reply reads
	// "llms/architecture/glossary → modules...".
	for _, m := range modules {
		files = append(files, struct {
			path    string
			content string
		}{
			filepath.Join(wikiDir, "modules", m.File),
			moduleDocSkeleton(m.Path),
		})
	}

	// Place .gitkeep only when there are no module files to
	// keep the directory tracked by git. When modules[] is
	// non-empty the directory is already populated.
	if len(modules) == 0 {
		files = append(files, struct {
			path    string
			content string
		}{
			filepath.Join(wikiDir, "modules", ".gitkeep"),
			gitkeepContent,
		})
	}

	// wiki.yml last: it is the source of truth for the next
	// /wiki update, so writing it after every other file
	// means a crash mid-init leaves a directory full of
	// content but no metadata — easy to spot, easy to clean.
	files = append(files, struct {
		path    string
		content string
	}{
		wikiYml, wikiYmlSkeletonWithModules(modules),
	})

	written = make([]string, 0, len(files))
	for _, f := range files {
		if err := atomicWrite(f.path, f.content); err != nil {
			return written, fmt.Errorf("write %s: %w", f.path, err)
		}
		rel, relErr := filepath.Rel(cwd, f.path)
		if relErr != nil {
			rel = f.path
		}
		written = append(written, rel)
	}
	return written, nil
}

// refuseIfPresent returns ErrWikiAlreadyExists when wikiDir
// is non-empty OR exists as a non-directory, OR when wikiYml
// already exists. An empty wikiDir is allowed.
//
// Symlinks: os.Stat follows them. A symlink at wikiDir that
// points to a non-empty directory triggers refusal — the
// user must remove the symlink before retrying. We do not
// want to silently write through a symlink and pollute the
// target.
func refuseIfPresent(wikiDir, wikiYml string) error {
	info, err := os.Stat(wikiDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No wiki dir yet — fine.
	case err != nil:
		return fmt.Errorf("stat wiki dir: %w", err)
	case !info.IsDir():
		return fmt.Errorf("%w: %s exists and is not a directory", ErrWikiAlreadyExists, wikiDir)
	default:
		entries, _ := os.ReadDir(wikiDir)
		if len(entries) > 0 {
			return fmt.Errorf("%w: %s contains %d entries (inspect and remove before retrying)",
				ErrWikiAlreadyExists, wikiDir, len(entries))
		}
	}

	if _, err := os.Stat(wikiYml); err == nil {
		return fmt.Errorf("%w: %s exists (remove before retrying)", ErrWikiAlreadyExists, wikiYml)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat wiki.yml: %w", err)
	}
	return nil
}

// atomicWrite writes content to a temp file adjacent to
// path, then renames into place. mode is 0o644 for content
// files and 0o600 for wiki.yml (matches the metadata-file
// convention used by ~/.nightme/gtw.yml).
//
// On any failure before the rename, the temp file is
// removed so cwd does not accumulate .new debris.
func atomicWrite(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wiki-init-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	mode := os.FileMode(0o644)
	if filepath.Base(path) == "wiki.yml" {
		mode = 0o600
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// projectName derives the wiki's display name from cwd's
// basename. Falls back to "Wiki" for the edge cases that
// should not occur with absolute paths ("" / "." / "/").
func projectName(cwd string) string {
	name := filepath.Base(cwd)
	if name == "" || name == "." || name == "/" {
		return "Wiki"
	}
	return name
}
