package core

import (
	"context"
	"os"
	"path/filepath"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/policy"
)

// missingRoots requires every registered root to be absent beneath an available,
// canonical workspace. Existing files, aliases and inaccessible paths are kept.
func (c *Core) missingRoots(roots []Root) bool {
	if len(roots) == 0 {
		return false
	}
	for _, root := range roots {
		missing := false
		for _, workspace := range c.pol.Workspaces {
			if root.Path == workspace || !policy.Within(workspace, root.Path) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(workspace)
			info, statErr := os.Stat(workspace)
			if err != nil || statErr != nil || resolved != workspace || !info.IsDir() {
				continue
			}
			for current := root.Path; current != workspace; current = filepath.Dir(current) {
				info, err := os.Lstat(current)
				if err == nil {
					resolved, resolveErr := filepath.EvalSymlinks(current)
					missing = current != root.Path && info.IsDir() && resolveErr == nil && resolved == current
					break
				}
				if !os.IsNotExist(err) {
					break
				}
				missing = filepath.Dir(current) == workspace
			}
			break
		}
		if !missing {
			return false
		}
	}
	return true
}

// PruneMissingProjects is service maintenance explicitly enabled by the owner.
// It uses normal retention tombstones and removes the enrollment atomically;
// source files, native transcripts, unrelated projects and backups are untouched.
func (c *Core) PruneMissingProjects(ctx context.Context) (int, error) {
	if !c.cfg.PruneMissingProjects {
		return 0, nil
	}
	owner := Caller{Roles: map[contract.Role]bool{contract.RoleAdmin: true}}
	projects, err := c.Projects(ctx, owner)
	if err != nil {
		return 0, err
	}
	if len(projects) >= 256 {
		return 0, contract.Errf(contract.CodeInvalid, "project cleanup requires a complete inventory")
	}
	removed := 0
	for _, project := range projects {
		if !c.missingRoots(project.Roots) {
			continue
		}
		req := contract.AdminRequest{Selector: "project", Target: project.ID}
		preview, err := c.RetentionPreview(ctx, owner, req)
		if err != nil {
			return removed, err
		}
		req.Confirm = preview.Confirm
		if _, err := c.retentionApply(ctx, owner, req, true); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
