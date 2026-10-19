package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/timfewi/ichnora/internal/contract"
)

// Applicability states, computed at read time and never stored.
const (
	ApCurrent   = "current"
	ApDurable   = "durable"
	ApUnchecked = "unchecked"
	ApStale     = "stale"
	ApExpired   = "expired"
	ApUnknown   = "unknown"
)

// maxToolCalls bounds external revalidation per request; beyond it the
// result is unknown rather than silently current.
const maxToolCalls = 8

// evaluator validates dependencies against current state for one request.
type evaluator struct {
	c        *Core
	ctx      context.Context
	now      string
	env      *Environment
	envGap   string
	roots    map[string]string
	files    map[string]fileCheck
	heads    map[string]headCheck
	tabula   map[string]json.RawMessage
	toolUsed int
}

type fileCheck struct {
	digest string
	reason string
}

type headCheck struct {
	head string
	err  error
}

func (c *Core) evaluator(ctx context.Context) (*evaluator, error) {
	env, gap := c.environment()
	e := &evaluator{c: c, ctx: ctx, now: c.now(), env: env, envGap: gap, roots: map[string]string{},
		files: map[string]fileCheck{}, heads: map[string]headCheck{}, tabula: map[string]json.RawMessage{}}
	rows, err := c.store.Read().QueryContext(ctx, `SELECT worktree_id, path FROM roots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var wt, path string
		if err := rows.Scan(&wt, &path); err != nil {
			return nil, err
		}
		e.roots[wt] = path
	}
	return e, rows.Err()
}

func (e *evaluator) file(root, rel string) fileCheck {
	key := root + "\x00" + rel
	if f, ok := e.files[key]; ok {
		return f
	}
	var f fileCheck
	abs, err := e.c.pol.Source(root, rel)
	switch {
	case err != nil:
		f.reason = "file unavailable: " + rel
	default:
		if d, err := digestFile(abs); err != nil {
			f.reason = "file cannot be digested: " + rel
		} else {
			f.digest = d
		}
	}
	e.files[key] = f
	return f
}

func (e *evaluator) head(root string) headCheck {
	if h, ok := e.heads[root]; ok {
		return h
	}
	h, err := gitHead(root)
	e.heads[root] = headCheck{h, err}
	return e.heads[root]
}

type verdict struct {
	state   string
	reasons []string
}

func (v *verdict) mark(state, reason string) {
	rank := map[string]int{ApCurrent: 0, ApUnknown: 1, ApStale: 2, ApExpired: 3}
	if rank[state] > rank[v.state] {
		v.state = state
	}
	v.reasons = append(v.reasons, reason)
}

// dep validates one stored dependency against the claim's worktree root.
func (e *evaluator) dep(v *verdict, root string, d contract.Ref) {
	switch d.Kind {
	case "file", "ast":
		if root == "" {
			v.mark(ApUnknown, "worktree root is no longer mapped")
			return
		}
		f := e.file(root, d.Path)
		switch {
		case f.reason != "":
			v.mark(ApStale, f.reason)
		case f.digest != d.SHA256:
			v.mark(ApStale, "file changed: "+d.Path)
		default:
			v.reasons = append(v.reasons, "file unchanged: "+d.Path)
		}
	case "revision":
		if root == "" {
			v.mark(ApUnknown, "worktree root is no longer mapped")
			return
		}
		h := e.head(root)
		switch {
		case h.err != nil:
			v.mark(ApUnknown, "revision unavailable")
		case h.head != d.Value:
			v.mark(ApStale, "revision changed")
		default:
			v.reasons = append(v.reasons, "revision unchanged")
		}
	case "environment":
		switch {
		case e.env == nil:
			v.mark(ApUnknown, "host applicability unknown: "+e.envGap)
		case e.env.ActivationEpoch != d.Epoch:
			v.mark(ApStale, "host environment epoch changed")
		default:
			v.reasons = append(v.reasons, "host environment epoch unchanged")
		}
	case "tabula":
		e.tabulaDep(v, root, d)
	default:
		v.mark(ApUnknown, "unsupported dependency kind")
	}
}

// tabulaDep re-reads the card section through Tabula and requires the same
// section id and fingerprint; a renamed or edited section is stale.
func (e *evaluator) tabulaDep(v *verdict, root string, d contract.Ref) {
	if root == "" || d.Path == "" {
		v.mark(ApUnknown, "tabula reference lacks a source path")
		return
	}
	key := root + "\x00" + d.Path
	out, ok := e.tabula[key]
	if !ok {
		if e.toolUsed >= maxToolCalls {
			v.mark(ApUnknown, "revalidation budget exhausted")
			return
		}
		e.toolUsed++
		res := e.c.tools.TabulaContext(e.ctx, root, []string{d.Path}, 65536)
		if res.Status != "ok" {
			v.mark(ApUnknown, "tabula revalidation: "+res.Status)
			e.tabula[key] = nil
			return
		}
		out = res.Output
		e.tabula[key] = out
	}
	if out == nil {
		v.mark(ApUnknown, "tabula revalidation unavailable")
		return
	}
	var ctx struct {
		Sections []struct {
			ID          string `json:"id"`
			Fingerprint string `json:"section_fingerprint"`
			Freshness   string `json:"freshness"`
		} `json:"sections"`
	}
	if json.Unmarshal(out, &ctx) != nil {
		v.mark(ApUnknown, "tabula output not understood")
		return
	}
	for _, s := range ctx.Sections {
		if s.ID == d.Section && s.Fingerprint == d.Fingerprint {
			if s.Freshness != "" && s.Freshness != "current" {
				v.mark(ApStale, "tabula section is "+s.Freshness)
				return
			}
			v.reasons = append(v.reasons, "tabula section unchanged")
			return
		}
	}
	// No exact match: the heading was renamed, the text changed, or the
	// section is gone. Each needs revalidation before reuse.
	v.mark(ApStale, "tabula section renamed, changed or no longer returned")
}

// claim computes a claim's applicability from its expiry and dependencies.
func (e *evaluator) claim(kind, worktree, expires string, deps []contract.Ref) verdict {
	v := verdict{state: ApCurrent}
	if expires != "" && expires <= e.now {
		v.mark(ApExpired, "expired")
	}
	if len(deps) == 0 {
		if v.state == ApExpired {
			return v
		}
		if kind == "decision" {
			return verdict{state: ApDurable, reasons: []string{"decision without declared dependencies"}}
		}
		return verdict{state: ApUnchecked, reasons: []string{"no dependencies declared; not validated"}}
	}
	root := e.roots[worktree]
	for _, d := range deps {
		e.dep(&v, root, d)
	}
	return v
}

// inputs checks a verification's tested inputs against the task's worktree.
func (e *evaluator) inputs(worktree string, in contract.VerificationInput) verdict {
	var deps []contract.Ref
	if in.Revision != "" {
		deps = append(deps, contract.Ref{Kind: "revision", Value: in.Revision})
	}
	for path, sha := range in.Files {
		deps = append(deps, contract.Ref{Kind: "file", Path: path, SHA256: sha})
	}
	if in.Epoch != "" {
		deps = append(deps, contract.Ref{Kind: "environment", Epoch: in.Epoch})
	}
	if len(deps) == 0 {
		return verdict{state: ApUnknown, reasons: []string{"no tested inputs recorded"}}
	}
	return e.claim("observation", worktree, "", deps)
}

// gitHead reads HEAD without running git: it follows `.git` files of
// worktrees, symbolic refs, the common directory and packed refs.
func gitHead(root string) (string, error) {
	gitdir := filepath.Join(root, ".git")
	st, err := os.Lstat(gitdir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		b, err := os.ReadFile(gitdir)
		if err != nil {
			return "", err
		}
		line := strings.TrimSpace(string(b))
		if !strings.HasPrefix(line, "gitdir: ") {
			return "", errors.New("unrecognized .git file")
		}
		gitdir = strings.TrimPrefix(line, "gitdir: ")
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(root, gitdir)
		}
	}
	head, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(string(head))
	if !strings.HasPrefix(h, "ref: ") {
		return h, nil
	}
	ref := strings.TrimPrefix(h, "ref: ")
	common := gitdir
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common = strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
	}
	for _, dir := range []string{gitdir, common} {
		if b, err := os.ReadFile(filepath.Join(dir, ref)); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	packed, err := os.ReadFile(filepath.Join(common, "packed-refs"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok && name == ref {
			return sha, nil
		}
	}
	return "", errors.New("ref not found")
}
