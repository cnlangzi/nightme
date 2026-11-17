// codefile.go — the code-file classification the simplify lens is
// scoped by.
//
// The simplify lens looks for reuse / simplification / efficiency /
// altitude in *code*. Prose, configuration and data files give it
// nothing to act on, so the simplify group covers the code files of
// the changeset only. The builtin and ocr groups keep their own scope —
// this filter narrows the simplify group alone.
//
// The classification is an allowlist: a path is a code file when its
// basename is a known code file name, or when its extension is a known
// code extension. A denylist would leak every format nobody thought of
// into the review, and binary files would need a separate detection
// step.

package agent

import (
	"path/filepath"
	"strings"
)

// codeExtensions are the extensions of code files — lowercase, without
// the leading dot. An extension that is not in this table is not a code
// file, whatever it holds.
var codeExtensions = map[string]bool{
	// scripting / systems
	"go": true, "py": true, "pyi": true, "rb": true, "php": true,
	"pl": true, "pm": true, "lua": true, "r": true, "jl": true,
	"tcl": true, "awk": true, "sed": true,
	// web
	"js": true, "jsx": true, "ts": true, "tsx": true, "mjs": true,
	"cjs": true, "mts": true, "cts": true, "vue": true, "svelte": true,
	"astro": true,
	// compiled / functional
	"rs": true, "zig": true, "nim": true, "cr": true, "d": true,
	"gd": true, "ex": true, "exs": true, "erl": true, "hrl": true,
	"hs": true, "lhs": true, "ml": true, "mli": true, "elm": true,
	"scala": true, "clj": true, "cljs": true, "cljc": true, "groovy": true,
	// jvm / mobile / dotnet
	"java": true, "kt": true, "kts": true, "swift": true, "dart": true,
	"cs": true, "fs": true, "fsx": true, "vb": true,
	// c family
	"c": true, "h": true, "cc": true, "cpp": true, "cxx": true,
	"hh": true, "hpp": true, "hxx": true, "m": true, "mm": true,
	// shells
	"sh": true, "bash": true, "zsh": true, "fish": true, "ps1": true,
	"psm1": true, "bat": true, "cmd": true,
	// schemas / infrastructure
	"sql": true, "proto": true, "graphql": true, "gql": true, "thrift": true,
	"tf": true, "tfvars": true, "bicep": true, "hcl": true,
	// markup / styles
	"html": true, "htm": true, "css": true, "scss": true, "sass": true,
	"less": true,
	// solidity / wasm / assembly / shaders
	"sol": true, "wat": true, "asm": true, "s": true, "glsl": true,
	"hlsl": true, "wgsl": true,
}

// codeFilenames are the basenames of code files whose extension says
// nothing about them — build files, container recipes, dependency
// manifests. The table is consulted before the extension table, so
// "CMakeLists.txt" is a code file although ".txt" is not.
var codeFilenames = map[string]bool{
	"Makefile": true, "GNUmakefile": true, "makefile": true,
	"Dockerfile": true, "Containerfile": true,
	"CMakeLists.txt": true, "BUILD": true, "BUILD.bazel": true,
	"WORKSPACE": true, "justfile": true,
	"Rakefile": true, "Gemfile": true, "Podfile": true, "Fastfile": true,
	"Jenkinsfile": true, "Vagrantfile": true, "Procfile": true, "Brewfile": true,
}

// isCodePath reports whether p is a code file.
//
// The filename table is consulted first: a known name wins over its
// extension (CMakeLists.txt is code, .txt is not). The extension match
// is case-insensitive.
func isCodePath(p string) bool {
	name := filepath.Base(p)
	if codeFilenames[name] {
		return true
	}
	ext := codeExtension(name)
	return ext != "" && codeExtensions[ext]
}

// codeFiles returns the code files of paths, preserving their order.
func codeFiles(paths []string) []string {
	var out []string
	for _, p := range paths {
		if isCodePath(p) {
			out = append(out, p)
		}
	}
	return out
}

// codeExtension returns the lowercased extension of a file name,
// without the leading dot. It is "" when the name carries no extension
// — including a dotfile like ".gitignore", whose leading dot is part of
// the name, not an extension separator.
func codeExtension(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}
