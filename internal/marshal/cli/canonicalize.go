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

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/marshal/engine"
	"code.vikunja.io/veans/internal/marshal/ledger"
	"code.vikunja.io/veans/internal/marshal/pathpattern"
	"code.vikunja.io/veans/internal/output"
)

// canonicalRow is one task's before-and-after.
type canonicalRow struct {
	TaskID     int64    `json:"task_id"`
	Identifier string   `json:"identifier"`
	Title      string   `json:"title"`
	WasOwned   []string `json:"was_paths_owned,omitempty"`
	Owned      []string `json:"paths_owned"`
	Affected   []string `json:"paths_affected"`
	Changed    bool     `json:"changed"`
	// Unresolved are paths this will not decide: the base is wrong and the
	// repository does not settle which file was meant. They are reported and
	// left exactly as they are.
	Unresolved []string `json:"unresolved,omitempty"`
	Applied    bool     `json:"applied"`
	Error      string   `json:"error,omitempty"`
}

// collision is two open tasks whose patterns name one file once both are
// spelled canonically. It was always true; nothing could see it.
type collision struct {
	Pattern string            `json:"pattern"`
	TaskIDs []int64           `json:"task_ids"`
	Was     map[string]string `json:"claimed_as"`
	Posted  bool              `json:"posted"`
}

func newCanonicalizeCmd() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "canonicalize",
		Short: "Rewrite stored scope paths into the canonical form, reporting collisions rather than merging them",
		Long: `Reads every open task's scope and rewrites each path into the canonical
form: relative to the repository root, one spelling per file. It writes
nothing unless --apply is passed, because the first run of this is a report
about how far the board has drifted and someone will want to read it before
anything is written.

A path relative to the app sub-directory is rebased onto the repository root
ONLY when the repository settles it: the rebased path exists and the path as
written does not. Anything else is left alone and listed under "unresolved".
Silently moving a claim onto a path nobody typed is how a lease lands on a
file nobody meant to claim.

Where two paths on DIFFERENT open tasks collapse onto one identity, that is a
real collision that was invisible until now: two tasks holding the same file,
each believing it was alone. It is never merged silently — it is reported,
posted as a comment on both tasks, and written to the ledger.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			roots, err := e.RepoRoots(ctx)
			if err != nil {
				return err
			}
			snap, err := e.Board.Snapshot(ctx)
			if err != nil {
				return err
			}

			repo := e.Board.Cfg.Repository
			rows := []canonicalRow{}
			// owner maps a canonical pattern onto the tasks now claiming it,
			// with the spelling each of them used to get there.
			owner := map[string]map[int64]string{}

			for _, t := range snap.Tasks {
				if t.Scope == nil {
					continue
				}
				row := canonicalRow{TaskID: t.ID, Identifier: t.Identifier, Title: t.Title, WasOwned: t.Scope.PathsOwned}
				var ownedChanged, affectedChanged bool
				var spelling map[string]string
				row.Owned, ownedChanged, spelling = canonicalizeList(t.Scope.PathsOwned, repo, roots, e.RepoRoot, &row)
				row.Affected, affectedChanged, _ = canonicalizeList(t.Scope.PathsAffected, repo, roots, e.RepoRoot, &row)
				row.Changed = ownedChanged || affectedChanged

				// Only paths_owned is leased, so only paths_owned can collide.
				for canonical, was := range spelling {
					if owner[canonical] == nil {
						owner[canonical] = map[int64]string{}
					}
					owner[canonical][t.ID] = was
				}
				rows = append(rows, row)
			}

			collisions := findCollisions(owner)

			applied := 0
			if apply {
				for i := range rows {
					if !rows[i].Changed {
						continue
					}
					if err := writeScope(ctx, e, snap.ByID[rows[i].TaskID], rows[i]); err != nil {
						rows[i].Error = err.Error()
						continue
					}
					rows[i].Applied = true
					applied++
					e.Log(ledger.Entry{
						Action:  "canonicalize",
						TaskID:  rows[i].TaskID,
						Subject: strings.Join(rows[i].Owned, ","),
						Outcome: "ok",
						Reason:  "rewritten to the canonical form",
					})
				}
				reportCollisions(ctx, e, collisions)
			}

			changed, unresolved := 0, 0
			for _, r := range rows {
				unresolved += len(r.Unresolved)
				if r.Changed {
					changed++
				}
			}
			if err := emit(cmd.OutOrStdout(), map[string]any{
				"apply":      apply,
				"roots":      roots.Roots,
				"app_root":   roots.AppRoot,
				"tasks":      rows,
				"changed":    changed,
				"applied":    applied,
				"unresolved": unresolved,
				"collisions": collisions,
			}); err != nil {
				return err
			}
			if len(collisions) > 0 {
				return output.New(output.CodeConflict, "%d path(s) are claimed by more than one open task once spelled canonically — real collisions that were invisible before; resolve them before anyone starts work", len(collisions))
			}
			if unresolved > 0 {
				return output.New(output.CodeConflict, "%d path(s) need a human: the base is wrong and the repository does not settle which file was meant", unresolved)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write the rewritten scopes (default: report only)")
	return cmd
}

func writeScope(ctx context.Context, e *engine.Engine, t *client.Task, row canonicalRow) error {
	scope := &client.TaskScope{PathsOwned: row.Owned, PathsAffected: row.Affected, Endpoints: []string{}}
	if t != nil && t.Scope != nil {
		scope.Endpoints = t.Scope.Endpoints
		scope.Notes = t.Scope.Notes
	}
	_, err := e.Board.Client.PutTaskScope(ctx, row.TaskID, scope)
	return err
}

// canonicalizeList rewrites one list, recording anything it will not decide.
// spelling maps each canonical pattern back to what the task actually had, so
// a collision report can say how each side spelled it.
func canonicalizeList(raw []string, repo string, roots pathpattern.Roots, repoRoot string, row *canonicalRow) (out []string, changed bool, spelling map[string]string) {
	out = make([]string, 0, len(raw))
	spelling = map[string]string{}
	for _, p := range raw {
		canonical, err := pathpattern.Canonical(p, repo)
		if err != nil {
			row.Unresolved = append(row.Unresolved, p)
			out = append(out, p)
			continue
		}
		if err := roots.Check(canonical); err != nil {
			rebased, ok := rebaseOntoAppRoot(canonical, roots, repoRoot)
			if !ok {
				row.Unresolved = append(row.Unresolved, p)
				out = append(out, p)
				continue
			}
			canonical = rebased
		}
		if canonical != p {
			changed = true
		}
		if _, dup := spelling[canonical]; dup {
			// Two spellings on ONE task collapsing is not a collision; it is
			// the same task saying the same thing twice. Fold it.
			changed = true
			continue
		}
		spelling[canonical] = p
		out = append(out, canonical)
	}
	return out, changed, spelling
}

// rebaseOntoAppRoot offers the app-root spelling only when the repository
// settles it: the rebased path exists and the path as written does not. That
// is the whole test, and it is deliberately narrow.
func rebaseOntoAppRoot(canonical string, roots pathpattern.Roots, repoRoot string) (string, bool) {
	candidate := roots.Suggest(canonical)
	if candidate == "" {
		return "", false
	}
	_, asWritten := pathpattern.SplitRepo(canonical)
	_, asRebased := pathpattern.SplitRepo(candidate)
	if pathExists(repoRoot, asWritten) || !pathExists(repoRoot, asRebased) {
		return "", false
	}
	return candidate, true
}

// findCollisions reports patterns that two open tasks arrive at from DIFFERENT
// spellings — the ones canonicalisation reveals.
//
// Two tasks that already claimed the identical string are not this. They have
// always been visibly on the same file, and the unordered-overlap invariant
// has been reporting them all along; repeating that here would bury the real
// finding under a pile of things everyone already knows, and a report that
// cries wolf on a clean board is a report nobody reads twice.
func findCollisions(owner map[string]map[int64]string) []collision {
	out := []collision{}
	for pattern, tasks := range owner {
		if len(tasks) < 2 || !spellingsDiffer(tasks) {
			continue
		}
		c := collision{Pattern: pattern, Was: map[string]string{}}
		for id := range tasks {
			c.TaskIDs = append(c.TaskIDs, id)
		}
		sort.Slice(c.TaskIDs, func(i, j int) bool { return c.TaskIDs[i] < c.TaskIDs[j] })
		for _, id := range c.TaskIDs {
			c.Was[fmt.Sprint(id)] = tasks[id]
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// spellingsDiffer reports whether the tasks reached this pattern by more than
// one route. One shared spelling means nothing was hidden.
func spellingsDiffer(tasks map[int64]string) bool {
	var first string
	for _, was := range tasks {
		if first == "" {
			first = was
			continue
		}
		if was != first {
			return true
		}
	}
	return false
}

// reportCollisions posts the finding on every task involved and lands a ledger
// entry. Both sides need to see it: each has been working as though it held
// the file alone, and only one of them can be right about that.
func reportCollisions(ctx context.Context, e *engine.Engine, collisions []collision) {
	for i := range collisions {
		c := &collisions[i]
		var others []string
		for _, id := range c.TaskIDs {
			others = append(others, fmt.Sprintf("#%d (claimed as <code>%s</code>)", id, c.Was[fmt.Sprint(id)]))
		}
		body := fmt.Sprintf(
			"<p><strong>Scope collision revealed by canonicalisation.</strong> "+
				"<code>%s</code> is claimed by %s.</p>"+
				"<p>These were different strings and therefore different claims, so neither task's lease could see the other. "+
				"Spelled canonically they are one file. Decide who owns it before anyone starts work.</p>",
			c.Pattern, strings.Join(others, " and "))
		posted := true
		for _, id := range c.TaskIDs {
			if _, err := e.Board.Client.AddTaskComment(ctx, id, body); err != nil {
				posted = false
			}
		}
		c.Posted = posted
		e.Log(ledger.Entry{
			Action:   "canonicalize",
			TaskID:   c.TaskIDs[0],
			Subject:  c.Pattern,
			Outcome:  "refused",
			Reason:   "two open tasks collapse onto one path; not merged",
			Metadata: map[string]any{"task_ids": c.TaskIDs, "claimed_as": c.Was},
		})
	}
}

func pathExists(repoRoot, rel string) bool {
	if rel == "" || strings.ContainsAny(rel, "*?[") {
		// A glob names no single file, so the repository cannot settle it.
		return false
	}
	_, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	return err == nil
}
