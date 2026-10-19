package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/ingest"
	"github.com/timfewi/ichnora/internal/policy"
	"github.com/timfewi/ichnora/internal/store"
)

// Project is an enrolled or withdrawn project.
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enrolled bool   `json:"enrolled"`
	Collect  bool   `json:"collect"`
	Recall   bool   `json:"recall"`
	Roots    []Root `json:"roots"`
}

// Root maps a path to a project worktree.
type Root struct {
	Path     string `json:"path"`
	Worktree string `json:"worktree"`
	Kind     string `json:"kind"`
}

func (c *Core) admin(caller Caller) error {
	if !caller.Has(contract.RoleAdmin) {
		return errForbidden
	}
	return nil
}

func (c *Core) projectByRef(ctx context.Context, q querier, ref string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE id = ? OR name = ?`, ref, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", contract.Errf(contract.CodeNotFound, "unknown project")
	}
	return id, err
}

func (c *Core) loadProject(ctx context.Context, q querier, id string) (Project, error) {
	var p Project
	if err := q.QueryRowContext(ctx, `SELECT id, name, enrolled, collect, recall FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Enrolled, &p.Collect, &p.Recall); err != nil {
		return p, err
	}
	rows, err := q.QueryContext(ctx, `SELECT path, worktree_id, kind FROM roots WHERE project_id = ? ORDER BY kind, path`, id)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	p.Roots = []Root{}
	for rows.Next() {
		var r Root
		if err := rows.Scan(&r.Path, &r.Worktree, &r.Kind); err != nil {
			return p, err
		}
		p.Roots = append(p.Roots, r)
	}
	return p, rows.Err()
}

// Enroll adds a project root or a worktree mapping. New projects default to
// collection and recall disabled unless requested.
func (c *Core) Enroll(ctx context.Context, caller Caller, req contract.AdminRequest) (Project, error) {
	if err := c.admin(caller); err != nil {
		return Project{}, err
	}
	root, err := c.pol.EnrollRoot(req.Path)
	if err != nil {
		return Project{}, err
	}
	if outer, ok, err := c.resolvePath(ctx, c.store.Read(), root); err != nil {
		return Project{}, err
	} else if ok && outer.Root != root && !policy.IsRepository(root) {
		return Project{}, contract.Errf(contract.CodeInvalid, "path is already covered by an enrolled project; only nested repositories enroll separately")
	}
	name := req.Name
	if name == "" {
		name = filepath.Base(root)
	}
	if len(name) > 64 || strings.ContainsAny(name, "/\x00") {
		return Project{}, contract.Errf(contract.CodeInvalid, "project name must be at most 64 bytes without slashes")
	}
	var id string
	err = c.write(ctx, func(tx *sql.Tx) error {
		var existing, kind string
		err := tx.QueryRowContext(ctx, `SELECT project_id, kind FROM roots WHERE path = ?`, root).Scan(&existing, &kind)
		switch {
		case err == nil:
			// Re-enrollment keeps identity and history; tombstones still apply.
			id = existing
			if req.WorktreeOf != "" {
				return contract.Errf(contract.CodeInvalid, "path is already mapped")
			}
			_, err = tx.ExecContext(ctx, `UPDATE projects SET enrolled = 1, withdrawn_at = NULL, collect = ?, recall = ? WHERE id = ?`,
				req.Collect, req.Recall, id)
			if err != nil {
				return err
			}
			return store.BumpGeneration(ctx, tx)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if req.WorktreeOf != "" {
			if id, err = c.projectByRef(ctx, tx, req.WorktreeOf); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO roots (path, project_id, worktree_id, kind, added_at) VALUES (?, ?, ?, 'worktree', ?)`,
				root, id, newID("wt_"), c.now())
			if err != nil {
				return err
			}
			return store.BumpGeneration(ctx, tx)
		}
		id = newID("prj_")
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, enrolled, collect, recall, created_at) VALUES (?, ?, 1, ?, ?, ?)`,
			id, name, req.Collect, req.Recall, c.now()); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return contract.Errf(contract.CodeInvalid, "project name already exists; choose another with name")
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO roots (path, project_id, worktree_id, kind, added_at) VALUES (?, ?, ?, 'primary', ?)`,
			root, id, newID("wt_"), c.now()); err != nil {
			return err
		}
		return store.BumpGeneration(ctx, tx)
	})
	if err != nil {
		return Project{}, err
	}
	return c.loadProject(ctx, c.store.Read(), id)
}

// Withdraw stops ingestion and recall for a project; history is retained.
func (c *Core) Withdraw(ctx context.Context, caller Caller, req contract.AdminRequest) (Project, error) {
	if err := c.admin(caller); err != nil {
		return Project{}, err
	}
	var id string
	err := c.write(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = c.projectByRef(ctx, tx, req.Project); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET enrolled = 0, collect = 0, recall = 0, withdrawn_at = ? WHERE id = ?`, c.now(), id); err != nil {
			return err
		}
		return store.BumpGeneration(ctx, tx)
	})
	if err != nil {
		return Project{}, err
	}
	return c.loadProject(ctx, c.store.Read(), id)
}

// Projects lists all projects.
func (c *Core) Projects(ctx context.Context, caller Caller) ([]Project, error) {
	if err := c.admin(caller); err != nil {
		return nil, err
	}
	rows, err := c.store.Read().QueryContext(ctx, `SELECT id FROM projects ORDER BY name LIMIT 256`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Project{}
	for _, id := range ids {
		p, err := c.loadProject(ctx, c.store.Read(), id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Candidate is a discovered repository and its enrollment state.
type Candidate struct {
	Path     string `json:"path"`
	Enrolled bool   `json:"enrolled"`
}

// DiscoverResult lists candidates without reading their contents.
type DiscoverResult struct {
	Candidates []Candidate `json:"candidates"`
	Truncated  bool        `json:"truncated"`
	Note       string      `json:"note"`
}

// Discover offers candidate repositories inside the workspace roots.
func (c *Core) Discover(ctx context.Context, caller Caller) (DiscoverResult, error) {
	if err := c.admin(caller); err != nil {
		return DiscoverResult{}, err
	}
	found, truncated := c.pol.Discover(4, 200)
	out := DiscoverResult{Candidates: []Candidate{}, Truncated: truncated,
		Note: "Candidates were located by .git presence only; nothing was read or enrolled."}
	for _, f := range found {
		s, ok, err := c.resolvePath(ctx, c.store.Read(), f.Path)
		if err != nil {
			return out, err
		}
		out.Candidates = append(out.Candidates, Candidate{Path: f.Path, Enrolled: ok && s.Root == f.Path})
	}
	return out, nil
}

// PolicyResult explains a source-policy decision.
type PolicyResult struct {
	Path     string `json:"path"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason"`
	Project  string `json:"project,omitempty"`
	Relative string `json:"relative,omitempty"`
}

// Policy inspects the source policy for a path or a transcript.
func (c *Core) Policy(ctx context.Context, caller Caller, req contract.AdminRequest) (PolicyResult, error) {
	if err := c.admin(caller); err != nil {
		return PolicyResult{}, err
	}
	out := PolicyResult{Path: req.Path}
	if req.Harness != "" {
		if _, err := c.pol.Transcript(req.Harness, req.Path); err != nil {
			out.Reason = policy.Reason(err)
			return out, nil
		}
		out.Allowed, out.Reason = true, "inside a configured session-source root; ingestion still requires an enrolled project with collection or an explicit import"
		return out, nil
	}
	abs, err := filepath.Abs(req.Path)
	if err != nil {
		return out, contract.Errf(contract.CodeInvalid, "invalid path")
	}
	s, _, err := c.enclosingRoot(ctx, c.store.Read(), filepath.Dir(abs))
	if err != nil {
		return out, err
	}
	if s.Root == "" {
		out.Reason = "not inside an enrolled project"
		return out, nil
	}
	rel, _ := filepath.Rel(s.Root, abs)
	out.Project, out.Relative = s.Project, rel
	if _, err := c.pol.Source(s.Root, rel); err != nil {
		out.Reason = policy.Reason(err)
		return out, nil
	}
	out.Allowed, out.Reason = true, "regular source file inside the project root"
	return out, nil
}

// Share is a cross-project sharing grant.
type Share struct {
	ID        string `json:"id"`
	ClaimID   string `json:"claim_id"`
	ToProject string `json:"to_project"`
	Revoked   bool   `json:"revoked"`
}

// ShareClaim explicitly shares one claim into another project.
func (c *Core) ShareClaim(ctx context.Context, caller Caller, req contract.AdminRequest) (Share, error) {
	if err := c.admin(caller); err != nil {
		return Share{}, err
	}
	sh := Share{ID: newID("shr_"), ClaimID: req.ClaimID}
	err := c.write(ctx, func(tx *sql.Tx) error {
		to, err := c.projectByRef(ctx, tx, req.ToProject)
		if err != nil {
			return err
		}
		sh.ToProject = to
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM claims WHERE id = ?`, req.ClaimID).Scan(&owner); err != nil {
			return contract.Errf(contract.CodeNotFound, "unknown claim")
		}
		if owner == to {
			return contract.Errf(contract.CodeInvalid, "claim already belongs to that project")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO shares (id, claim_id, to_project, origin, created_at) VALUES (?, ?, ?, ?, ?)`,
			sh.ID, req.ClaimID, to, mustJSON(map[string]string{"role": "admin"}), c.now()); err != nil {
			return err
		}
		return store.BumpGeneration(ctx, tx)
	})
	return sh, err
}

// Revoke ends a share; cached IDs and cursors stop resolving immediately.
func (c *Core) Revoke(ctx context.Context, caller Caller, req contract.AdminRequest) (Share, error) {
	if err := c.admin(caller); err != nil {
		return Share{}, err
	}
	var sh Share
	err := c.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE shares SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, c.now(), req.ShareID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return contract.Errf(contract.CodeNotFound, "unknown or already revoked share")
		}
		if err := tx.QueryRowContext(ctx, `SELECT id, claim_id, to_project FROM shares WHERE id = ?`, req.ShareID).
			Scan(&sh.ID, &sh.ClaimID, &sh.ToProject); err != nil {
			return err
		}
		sh.Revoked = true
		return store.BumpGeneration(ctx, tx)
	})
	return sh, err
}

// BackupResult reports a written snapshot.
type BackupResult struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Policy string `json:"policy"`
}

// Backup writes a consistent snapshot outside source trees and the Nix store.
func (c *Core) Backup(ctx context.Context, caller Caller, req contract.AdminRequest) (BackupResult, error) {
	if err := c.admin(caller); err != nil {
		return BackupResult{}, err
	}
	out := filepath.Clean(req.Out)
	if !filepath.IsAbs(out) || strings.HasPrefix(out, "/nix/store") {
		return BackupResult{}, contract.Errf(contract.CodeInvalid, "backup path must be absolute and outside the Nix store")
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(out))
	if err != nil {
		return BackupResult{}, contract.Errf(contract.CodeInvalid, "backup directory does not exist")
	}
	for _, w := range c.pol.Workspaces {
		if policy.Within(w, dir) {
			return BackupResult{}, contract.Errf(contract.CodePolicyDenied, "backups must stay outside source workspaces")
		}
	}
	n, err := c.store.Backup(ctx, filepath.Join(dir, filepath.Base(out)))
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return BackupResult{}, contract.Errf(contract.CodeInvalid, "backup target already exists")
		}
		return BackupResult{}, err
	}
	return BackupResult{Path: filepath.Join(dir, filepath.Base(out)), Bytes: n,
		Policy: "Backups contain every retained record, including later-deleted data; deletion does not reach existing backups."}, nil
}

// Environment is the optional host descriptor.
type Environment struct {
	Format          string            `json:"format"`
	SystemIdentity  string            `json:"system_identity,omitempty"`
	PolicyBaseline  string            `json:"policy_baseline,omitempty"`
	HarnessVersions map[string]string `json:"harness_versions,omitempty"`
	ActivationEpoch string            `json:"activation_epoch"`
}

// environment reads the host descriptor; absence is reported, not invented.
func (c *Core) environment() (*Environment, string) {
	if c.cfg.EnvironmentDescriptor == "" {
		return nil, "no environment descriptor configured"
	}
	f, err := os.Open(c.cfg.EnvironmentDescriptor)
	if err != nil {
		return nil, "environment descriptor unavailable"
	}
	defer f.Close()
	var e Environment
	dec := json.NewDecoder(&limitReader{f, 64 << 10})
	dec.DisallowUnknownFields()
	if dec.Decode(&e) != nil || e.Format != contract.EnvironmentSpec || e.ActivationEpoch == "" || len(e.ActivationEpoch) > 128 {
		return nil, "environment descriptor is invalid"
	}
	return &e, ""
}

type limitReader struct {
	f interface{ Read([]byte) (int, error) }
	n int
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("descriptor too large")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.f.Read(p)
	l.n -= n
	return n, err
}

// EnvironmentResult reports the current descriptor.
type EnvironmentResult struct {
	Descriptor *Environment `json:"descriptor"`
	Gap        string       `json:"gap,omitempty"`
	Recorded   bool         `json:"recorded"`
}

// EnvironmentStatus records a newly seen epoch for history and reports it.
func (c *Core) EnvironmentStatus(ctx context.Context, caller Caller) (EnvironmentResult, error) {
	if err := c.admin(caller); err != nil {
		return EnvironmentResult{}, err
	}
	env, gap := c.environment()
	out := EnvironmentResult{Descriptor: env, Gap: gap}
	if env == nil {
		return out, nil
	}
	err := c.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO environments (epoch, descriptor, first_seen) VALUES (?, ?, ?)`,
			env.ActivationEpoch, mustJSON(env), c.now())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		out.Recorded = n > 0
		return nil
	})
	return out, err
}

// digestFile hashes a policy-checked file, bounded to 16 MiB.
func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil {
		return "", err
	}
	if len(b) > 16<<20 {
		return "", errors.New("file too large to digest")
	}
	return ingest.Digest(b), nil
}
