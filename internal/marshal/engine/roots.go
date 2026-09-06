// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"code.vikunja.io/veans/internal/marshal/codeowners"
	"code.vikunja.io/veans/internal/marshal/pathpattern"
	"code.vikunja.io/veans/internal/marshal/worktree"
)

// RepoRoots reads the repository's top-level entries — the answer to "what
// does a repository-root-relative path start with here".
//
// The board cannot work this out. It has no checkout, so "src/db/schema.ts"
// and "captain-yard-web/src/db/schema.ts" are equally plausible spellings of
// one file to it, and it accepted both — which is how one file came to have
// two live leases held by two tasks that could not see each other. Marshal has
// the checkout, so Marshal is what tells it: PublishRepoRoots puts this on the
// project and the board enforces against it from then on.
//
// It reads the tree, not the directory. A claim on an untracked or ignored
// path is not a claim on anything, and node_modules has no business widening
// what the board will accept.
func (e *Engine) RepoRoots(ctx context.Context) (pathpattern.Roots, error) {
	entries, err := worktree.TopLevelEntries(ctx, e.RepoRoot, "")
	if err != nil {
		return pathpattern.Roots{}, err
	}
	roots := pathpattern.Roots{
		Roots:   entries,
		AppRoot: strings.Trim(strings.TrimSpace(e.Cfg.AppRoot), "/"),
	}
	if roots.AppRoot == "" {
		// No app root, no ambiguity: every path is relative to the one base
		// there is, and nothing needs refusing.
		return roots, nil
	}
	// The app root's own entries are the ambiguity. "src" here and no "src" at
	// the repository root means a path starting "src/" was written as the
	// application sees it, and is a second identity for a file already claimed
	// from the repository root.
	appEntries, err := worktree.TopLevelEntries(ctx, e.RepoRoot, roots.AppRoot)
	if err != nil {
		// An app_root that is not in the tree yet: publish the roots without
		// the ambiguity set rather than failing, and enforce nothing until it
		// is real.
		return roots, nil //nolint:nilerr // an absent app root disables the check, it does not break publishing
	}
	roots.AppEntries = appEntries
	return roots, nil
}

// PublishRepoRoots writes the repository's shape onto the board so the board
// can refuse a scope path relative to the wrong base. It is a no-op when the
// board already agrees, so it is safe to call on every poll.
//
// A repository with no commits, or one whose app_root is not actually in the
// tree, publishes nothing rather than something wrong. Enforcement that
// refuses valid paths would be worse than none: it would teach everyone that
// the check is noise.
func (e *Engine) PublishRepoRoots(ctx context.Context) (roots pathpattern.Roots, changed bool, err error) {
	roots, err = e.RepoRoots(ctx)
	if err != nil {
		return roots, false, err
	}
	if !roots.Declared() {
		return roots, false, nil
	}
	if roots.AppRoot != "" && !slices.Contains(roots.Roots, roots.AppRoot) {
		return roots, false, fmt.Errorf("app_root %q is not a top-level entry of the repository; refusing to publish a shape the board would enforce wrongly", roots.AppRoot)
	}

	project, err := e.Board.Client.GetProject(ctx, e.Board.Cfg.ProjectID)
	if err != nil {
		return roots, false, err
	}
	if project.ScopeRepoRoots == roots.String() &&
		project.ScopeAppRoot == roots.AppRoot &&
		project.ScopeAppEntries == roots.AppEntriesString() {
		return roots, false, nil
	}
	if _, err := e.Board.Client.PatchProject(ctx, e.Board.Cfg.ProjectID, map[string]any{
		"scope_repo_roots":  roots.String(),
		"scope_app_root":    roots.AppRoot,
		"scope_app_entries": roots.AppEntriesString(),
	}); err != nil {
		return roots, false, err
	}
	return roots, true, nil
}

// chokepointPatterns reads the CODEOWNERS patterns alone, for the invariant
// that asks whether a claim could ever reach them. An unreadable or absent
// file is not a finding here — Chokepoints already reports that — so it yields
// nothing rather than a second complaint about the same thing.
func (e *Engine) chokepointPatterns() []string {
	b, err := os.ReadFile(filepath.Join(e.RepoRoot, e.Cfg.Codeowners)) //nolint:gosec // repository file named in the user's config
	if err != nil {
		return nil
	}
	f, err := codeowners.Parse(b)
	if err != nil {
		return nil
	}
	return f.Chokepoints()
}
