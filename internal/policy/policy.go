// Package policy decides which paths Ichnora may enroll or read. Every check
// resolves symlinks at access time and fails closed.
package policy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Denied explains a refusal without echoing file content.
type Denied struct{ Reason string }

func (d *Denied) Error() string { return "policy denied: " + d.Reason }

func deny(reason string) error { return &Denied{Reason: reason} }

// Excluded directory names and their categories. They apply below workspace
// and project roots; a project may not be enrolled inside one either.
var excludedDirs = map[string]string{
	".git": "version-control metadata", ".hg": "version-control metadata", ".svn": "version-control metadata",
	"node_modules": "dependency tree", "vendor": "dependency tree", ".venv": "dependency tree", "venv": "dependency tree",
	"target": "build output", "dist": "build output", "build": "build output", "result": "build output",
	".direnv": "generated environment", "__pycache__": "generated cache", ".cache": "generated cache",
	".ruff_cache": "generated cache", ".mypy_cache": "generated cache", ".pytest_cache": "generated cache",
	".ast-index": "private runtime data", ".ast-index-map": "private runtime data",
	"archive": "archive", "_archived": "archive", "mirrors": "mirror",
	".ssh": "credential store", ".gnupg": "credential store", "secrets": "credential store", ".password-store": "credential store",
}

// Excluded file name patterns (filepath.Match against the base name).
var excludedFiles = []struct{ pattern, category string }{
	{".env", "credential store"}, {".env.*", "credential store"}, {"*.pem", "credential store"},
	{"*.key", "credential store"}, {"id_rsa*", "credential store"}, {"id_ed25519*", "credential store"},
	{"id_ecdsa*", "credential store"}, {".netrc", "credential store"}, {"*.age", "credential store"},
	{"*.kdbx", "credential store"}, {"*.p12", "credential store"}, {"*.pfx", "credential store"},
	{"credentials*", "credential store"}, {"*.token", "credential store"},
	{"*.sqlite", "private runtime data"}, {"*.sqlite3", "private runtime data"}, {"*.db", "private runtime data"},
	{"*.db-wal", "private runtime data"}, {"*.db-shm", "private runtime data"},
	{"result-*", "build output"}, {"*.tar", "archive"}, {"*.tar.*", "archive"}, {"*.tgz", "archive"},
	{"*.zip", "archive"}, {"*.7z", "archive"},
}

// Policy holds the configured permissions. Paths are resolved absolute paths.
type Policy struct {
	Workspaces     []string
	SessionSources map[string][]string
	StateDir       string
}

// New resolves the configured roots. Missing roots are dropped, not created.
func New(workspaces []string, sessions map[string][]string, stateDir string) *Policy {
	p := &Policy{SessionSources: map[string][]string{}, StateDir: stateDir}
	for _, w := range workspaces {
		if r, err := resolveDir(w); err == nil {
			p.Workspaces = append(p.Workspaces, r)
		}
	}
	for harness, roots := range sessions {
		for _, root := range roots {
			if r, err := resolveDir(root); err == nil {
				p.SessionSources[harness] = append(p.SessionSources[harness], r)
			}
		}
	}
	return p
}

func resolveDir(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", deny("path must be absolute")
	}
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", deny("path does not exist")
	}
	st, err := os.Stat(r)
	if err != nil || !st.IsDir() {
		return "", deny("path is not a directory")
	}
	return r, nil
}

// Within reports whether path equals root or lies below it.
func Within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

func excludedComponent(rel string) (string, bool) {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if cat, ok := excludedDirs[part]; ok {
			return cat, true
		}
	}
	return "", false
}

func excludedFile(name string) (string, bool) {
	for _, e := range excludedFiles {
		if ok, _ := filepath.Match(e.pattern, name); ok {
			return e.category, true
		}
	}
	return "", false
}

// EnrollRoot validates a project root: inside a workspace, not the workspace
// itself, not inside an excluded tree and not the state directory.
func (p *Policy) EnrollRoot(path string) (string, error) {
	r, err := resolveDir(path)
	if err != nil {
		return "", err
	}
	if p.StateDir != "" && (Within(p.StateDir, r) || Within(r, p.StateDir)) {
		return "", deny("state directory cannot be enrolled")
	}
	for _, w := range p.Workspaces {
		if r == w {
			return "", deny("a workspace root is a boundary, not a project")
		}
		if Within(w, r) {
			rel, _ := filepath.Rel(w, r)
			if cat, bad := excludedComponent(rel); bad {
				return "", deny("path lies in an excluded " + cat)
			}
			return r, nil
		}
	}
	return "", deny("path is outside the configured workspace roots")
}

// IsRepository reports whether dir is the top of a Git checkout or worktree.
func IsRepository(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// Source resolves a project-relative source path for reading. It rejects
// absolute and parent paths, excluded components and names, nested
// repositories and symlinks whose target leaves the project root.
func (p *Policy) Source(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", deny("source path must be project-relative")
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", deny("source path escapes the project root")
	}
	if cat, bad := excludedComponent(clean); bad {
		return "", deny("source path lies in an excluded " + cat)
	}
	if cat, bad := excludedFile(filepath.Base(clean)); bad {
		return "", deny("source file is an excluded " + cat)
	}
	// Nested repositories are separate projects unless enrolled themselves.
	dir := root
	parts := strings.Split(filepath.ToSlash(filepath.Dir(clean)), "/")
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		if IsRepository(dir) {
			return "", deny("source path lies in a nested repository")
		}
	}
	full := filepath.Join(root, clean)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", deny("source path does not exist")
	}
	if !Within(root, resolved) {
		return "", deny("source path resolves outside the project root")
	}
	if relResolved, _ := filepath.Rel(root, resolved); relResolved != clean {
		if cat, bad := excludedComponent(relResolved); bad {
			return "", deny("source target lies in an excluded " + cat)
		}
		if cat, bad := excludedFile(filepath.Base(resolved)); bad {
			return "", deny("source target is an excluded " + cat)
		}
		for dir := filepath.Dir(resolved); dir != root && Within(root, dir); dir = filepath.Dir(dir) {
			if IsRepository(dir) {
				return "", deny("source target lies in a nested repository")
			}
		}
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		return "", deny("source path is not a regular file")
	}
	return resolved, nil
}

// Transcript validates a native transcript path for a harness: a regular,
// non-symlink JSON/JSONL file whose resolved location is inside a configured
// session-source root. Membership in a root grants no other file.
func (p *Policy) Transcript(harness, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", deny("transcript path must be absolute")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return "", deny("transcript does not exist")
	}
	if st.Mode()&fs.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return "", deny("transcript must be a regular file, not a symlink")
	}
	if ext := filepath.Ext(path); ext != ".jsonl" && ext != ".json" {
		return "", deny("transcript must be a .jsonl or .json file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", deny("transcript does not resolve")
	}
	for _, root := range p.SessionSources[harness] {
		if Within(root, resolved) && resolved != root {
			return resolved, nil
		}
	}
	return "", deny("transcript lies outside the configured session-source roots")
}

// Candidate is a discovered, unread repository.
type Candidate struct {
	Path string `json:"path"`
}

// Discover lists Git repositories below the workspace roots without reading
// their contents. It skips excluded trees, does not descend into found
// repositories and stops at the given limits.
func (p *Policy) Discover(maxDepth, maxResults int) ([]Candidate, bool) {
	var out []Candidate
	truncated := false
	for _, w := range p.Workspaces {
		var walk func(dir string, depth int)
		walk = func(dir string, depth int) {
			if len(out) >= maxResults {
				truncated = true
				return
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			for _, e := range entries {
				if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
					continue
				}
				if _, bad := excludedDirs[e.Name()]; bad || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				child := filepath.Join(dir, e.Name())
				if IsRepository(child) {
					if len(out) >= maxResults {
						truncated = true
						return
					}
					out = append(out, Candidate{Path: child})
					continue
				}
				if depth < maxDepth {
					walk(child, depth+1)
				}
			}
		}
		walk(w, 1)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, truncated
}

// Reason extracts the denial reason, or "" for other errors.
func Reason(err error) string {
	var d *Denied
	if errors.As(err, &d) {
		return d.Reason
	}
	return ""
}
