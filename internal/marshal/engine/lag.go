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
	"sort"
	"strings"
	"time"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/marshal/board"
	"code.vikunja.io/veans/internal/marshal/discord"
	"code.vikunja.io/veans/internal/marshal/ledger"
	"code.vikunja.io/veans/internal/marshal/notify"
	"code.vikunja.io/veans/internal/marshal/pathpattern"
	"code.vikunja.io/veans/internal/marshal/worktree"
)

// LagReport is one pass of lag computation over the claimed branches.
type LagReport struct {
	Base     string       `json:"base"`
	BaseSHA  string       `json:"base_sha"`
	Branches []*BranchLag `json:"branches"`
	Skipped  []LagSkipped `json:"skipped,omitempty"`
	Cached   int          `json:"cached"`
	Errors   []string     `json:"errors,omitempty"`
}

// LagSkipped records a task that could not be measured, and why. A task that
// cannot be measured must not silently look like a task with no lag.
type LagSkipped struct {
	TaskID int64  `json:"task_id"`
	Branch string `json:"branch,omitempty"`
	Reason string `json:"reason"`
}

// BranchLag is one task's lag, in the shape the board stores.
type BranchLag struct {
	TaskRef
	Lag *client.TaskLag `json:"lag"`
}

// lagCacheEntry is what a computation is keyed on. Lag is a pure function of
// the branch tip, the base tip and the task's scope, so nothing needs
// recomputing until one of those moves — and on the common poll, where none
// has, this does no git work at all.
type lagCacheEntry struct {
	branchTip string
	baseTip   string
	scopeRev  string
	lag       *client.TaskLag
}

// ComputeLag measures every claimed task's branch against the integration
// branch, scoped to what the task claims.
//
// This is the same walk Reconcile already does — fetch, then compare a claimed
// branch against the base — with a different comparison, so the marginal cost
// on a poll that has already fetched is close to zero.
func (e *Engine) ComputeLag(ctx context.Context, snap *board.Snapshot, base string) (*LagReport, error) {
	if base == "" {
		base = e.Cfg.IntegrationBranch
	}
	rep := &LagReport{Base: base, Branches: []*BranchLag{}}

	// Fetch every branch, not just the base. A worker's branch lives on the
	// remote; without this it is absent from Marshal's checkout and comes back
	// "cannot be measured", which is honest but useless — the whole point is
	// to measure it. Failure is tolerated: a stale checkout still measures
	// what it already has.
	_ = worktree.Fetch(ctx, e.RepoRoot, worktree.RemoteOf(base))

	baseTip, err := worktree.ResolveSHA(ctx, e.RepoRoot, base)
	if err != nil || baseTip == "" {
		// Without a base there is nothing to be behind. Say so rather than
		// reporting every branch as current.
		return nil, fmt.Errorf("resolve the integration branch %q: %w", base, err)
	}
	rep.BaseSHA = baseTip

	for _, t := range snap.Tasks {
		branch := board.BranchOf(t)
		if branch == "" {
			continue
		}
		if t.Scope == nil || (len(t.Scope.PathsOwned) == 0 && len(t.Scope.PathsAffected) == 0) {
			rep.Skipped = append(rep.Skipped, LagSkipped{TaskID: t.ID, Branch: branch, Reason: "the task claims no paths, so there is nothing for the base to move under"})
			continue
		}
		// A branch the worker pushed lives on the remote, so look there too
		// before giving up. Marshal never checks these out; it only reads them.
		branchTip, err := worktree.ResolveSHA(ctx, e.RepoRoot, branch)
		if err != nil || branchTip == "" {
			remoteRef := worktree.RemoteOf(base) + "/" + branch
			if tip, rerr := worktree.ResolveSHA(ctx, e.RepoRoot, remoteRef); rerr == nil && tip != "" {
				branch, branchTip = remoteRef, tip
			}
		}
		if branchTip == "" {
			rep.Skipped = append(rep.Skipped, LagSkipped{TaskID: t.ID, Branch: branch, Reason: "no such branch here or on " + worktree.RemoteOf(base)})
			continue
		}

		if cached, ok := e.cachedLag(t, branchTip, baseTip); ok {
			rep.Cached++
			if cached != nil {
				rep.Branches = append(rep.Branches, &BranchLag{TaskRef: refOf(t), Lag: cached})
			}
			continue
		}

		lag, err := e.lagFor(ctx, snap, t, branch, base, baseTip)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("#%d (%s): %v", t.ID, branch, err))
			continue
		}
		e.storeLagCache(t, branchTip, baseTip, lag)
		if lag != nil {
			rep.Branches = append(rep.Branches, &BranchLag{TaskRef: refOf(t), Lag: lag})
		}
	}
	sort.Slice(rep.Branches, func(i, j int) bool { return rep.Branches[i].TaskID < rep.Branches[j].TaskID })
	return rep, nil
}

// lagFor measures one branch. It returns nil when the branch is not behind at
// all, which is what lets an absent record mean "current" rather than "never
// looked".
func (e *Engine) lagFor(ctx context.Context, snap *board.Snapshot, t *client.Task, branch, base, baseTip string) (*client.TaskLag, error) {
	mergeBase, err := worktree.MergeBase(ctx, e.RepoRoot, base, branch)
	if err != nil {
		return nil, err
	}
	if mergeBase == "" || mergeBase == baseTip {
		return nil, nil // no shared history, or already current
	}
	behind, err := worktree.CommitsBehind(ctx, e.RepoRoot, base, branch)
	if err != nil {
		return nil, err
	}
	if behind == 0 {
		return nil, nil
	}
	moved, err := worktree.MovedFiles(ctx, e.RepoRoot, mergeBase, baseTip)
	if err != nil {
		return nil, err
	}

	lag := &client.TaskLag{
		Branch: branch, Base: base, BaseSHA: baseTip, MergeBaseSHA: mergeBase,
		CommitsBehind: behind, Collisions: []*client.LagCollision{},
		ComputedAt: time.Now().UTC(),
	}
	repo := e.Board.Cfg.Repository
	for _, f := range pathpattern.CanonicalFiles(moved) {
		severity := severityFor(t.Scope, f, repo)
		if severity == client.LagSeverityElsewhere {
			// Counted in commits_behind and nothing more. Listing every file
			// the base moved would bury the two that matter.
			continue
		}
		c := &client.LagCollision{Path: f, Severity: severity}
		if landing, err := worktree.LastLanding(ctx, e.RepoRoot, mergeBase, baseTip, f); err == nil && landing != nil {
			c.LandedInSHA, c.LandedAt = landing.SHA, landing.At
			resolveLanded(snap, c, landing.Refs)
		}
		lag.Collisions = append(lag.Collisions, c)
	}
	sort.Slice(lag.Collisions, func(i, j int) bool { return lag.Collisions[i].Path < lag.Collisions[j].Path })
	lag.Severity = client.MaxLagSeverity(lag.Collisions)
	if lag.Severity == "" {
		lag.Severity = client.LagSeverityElsewhere
	}
	return lag, nil
}

// severityFor decides which of the task's path sets a moved file landed in.
// paths_owned wins: a file the task both owns and reads is a certain conflict,
// not a stale assumption.
func severityFor(scope *client.TaskScope, file, repo string) string {
	if scope == nil {
		return client.LagSeverityElsewhere
	}
	if coveredByAny(scope.PathsOwned, file, repo) {
		return client.LagSeverityOwned
	}
	if coveredByAny(scope.PathsAffected, file, repo) {
		return client.LagSeverityAffected
	}
	return client.LagSeverityElsewhere
}

func coveredByAny(patterns []string, file, repo string) bool {
	// The file comes from git and is repository-root-relative; a pattern may
	// carry a repo: namespace, so put the file in the same namespace before
	// comparing rather than comparing across two dialects.
	namespaced, err := pathpattern.Canonical(file, repo)
	if err != nil {
		namespaced = file
	}
	for _, p := range patterns {
		if pathpattern.Covers(p, namespaced) {
			return true
		}
	}
	return false
}

// resolveLanded turns a Refs: trailer into the task that landed the change.
//
// It resolves against the snapshot rather than the network: the task that
// landed a change is almost always one that just merged, which the snapshot
// already carries among its done tasks. A ref that resolves to nothing leaves
// the collision with its sha and no task, which is the honest answer for a
// hand-pushed commit — still lag, just not attributable.
func resolveLanded(snap *board.Snapshot, c *client.LagCollision, refs []string) {
	for _, ref := range refs {
		t := snap.ByRef(ref)
		if t == nil {
			continue
		}
		c.LandedByTaskID = t.ID
		c.LandedByIdentifier = t.Identifier
		if c.LandedByIdentifier == "" {
			c.LandedByIdentifier = ref
		}
		return
	}
}

func (e *Engine) cachedLag(t *client.Task, branchTip, baseTip string) (*client.TaskLag, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.lagCache[t.ID]
	if !ok {
		return nil, false
	}
	if entry.branchTip != branchTip || entry.baseTip != baseTip || entry.scopeRev != scopeRevision(t) {
		return nil, false
	}
	return entry.lag, true
}

func (e *Engine) storeLagCache(t *client.Task, branchTip, baseTip string, lag *client.TaskLag) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lagCache == nil {
		e.lagCache = map[int64]lagCacheEntry{}
	}
	e.lagCache[t.ID] = lagCacheEntry{branchTip: branchTip, baseTip: baseTip, scopeRev: scopeRevision(t), lag: lag}
}

// scopeRevision is a cheap identity for the task's claim. Widening a scope
// changes what counts as a collision without moving either sha, so it belongs
// in the cache key.
func scopeRevision(t *client.Task) string {
	if t.Scope == nil {
		return ""
	}
	return strings.Join(t.Scope.PathsOwned, "\x00") + "\x1e" + strings.Join(t.Scope.PathsAffected, "\x00")
}

// PublishLag writes the measured lag onto the board and clears the record for
// every claimed branch that is no longer behind, so an absent record means
// "current" rather than "never looked".
//
// The label follows the blocking severity only. `affected` and `elsewhere` are
// reported in the record and on the readiness row; labelling them too would
// put a marshal:lag chip on most of the board and teach everyone to ignore it.
func (e *Engine) PublishLag(ctx context.Context, snap *board.Snapshot, rep *LagReport) (written, cleared int) {
	behind := map[int64]*client.TaskLag{}
	for _, b := range rep.Branches {
		behind[b.TaskID] = b.Lag
	}

	for _, b := range rep.Branches {
		if _, err := e.Board.Client.PutTaskLag(ctx, b.TaskID, b.Lag); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("#%d: record lag: %v", b.TaskID, err))
			continue
		}
		written++
		t := snap.ByID[b.TaskID]
		e.applyLagLabel(ctx, t, b.Lag.Blocking())
		e.announceLag(ctx, t, b.Lag)
	}

	// A branch that has caught up must stop being reported as behind. This is
	// the half that makes the record trustworthy: without it the board keeps
	// yesterday's answer and every consumer of it is wrong in the direction
	// that costs work.
	for _, t := range snap.Tasks {
		if _, still := behind[t.ID]; still || board.BranchOf(t) == "" {
			continue
		}
		if err := e.Board.Client.DeleteTaskLag(ctx, t.ID); err == nil {
			cleared++
		}
		e.applyLagLabel(ctx, t, false)
		// Forget the severity so a branch that falls behind again is carded
		// again. Without this, catching up would silently disarm the alarm.
		e.flags.Clear(lagFlagKey(t.ID))
	}
	return written, cleared
}

// announceLag posts a Discord card when a task crosses INTO owned, and only
// then. Not on affected, not on elsewhere, and not again while the severity is
// unchanged.
//
// A channel that fires on every poll is a channel everyone mutes, and the
// owned card is then lost along with it. The moment worth interrupting someone
// for is the one where a conflict stopped being hypothetical.
func (e *Engine) announceLag(ctx context.Context, t *client.Task, lag *client.TaskLag) {
	if t == nil || lag == nil {
		return
	}
	// Seen records the new severity and reports whether it was already that,
	// so this is true exactly on a change.
	changed := !e.flags.Seen(lagFlagKey(t.ID), lag.Severity)
	if !changed || !lag.Blocking() {
		return
	}
	owned := lag.OwnedCollisions()
	files := make([]string, 0, len(owned))
	for i, c := range owned {
		if i == 6 {
			files = append(files, fmt.Sprintf("…and %d more", len(owned)-6))
			break
		}
		files = append(files, fmt.Sprintf("%s — landed by %s", c.Path, landedByLabel(c)))
	}
	summary := fmt.Sprintf("Marshal: %s is %d commit%s behind %s in %d file%s it owns. A conflict is certain; review will refuse it.",
		t.Identifier, lag.CommitsBehind, plural(lag.CommitsBehind), lag.Base, len(owned), plural(len(owned)))
	e.log(ledger.Entry{Action: "lag", TaskID: t.ID, Subject: lag.Branch, Outcome: "blocking", Reason: "the base moved inside a path this task owns"})
	e.Notify(ctx, "lag", e.Format.FromFinding(notify.Finding{
		Kind: "lag", TaskID: t.ID, Identifier: t.Identifier, Title: t.Title, Summary: summary,
		Details: []discord.Field{{Name: "Files", Value: strings.Join(files, "\n")}},
	}))
}

func lagFlagKey(taskID int64) string { return fmt.Sprintf("lag:%d", taskID) }

// landedByLabel names who moved a file, keeping the sha when no Refs: trailer
// attributed it to a task.
func landedByLabel(c *client.LagCollision) string {
	sha := c.LandedInSHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	switch {
	case c.LandedByIdentifier != "" && sha != "":
		return fmt.Sprintf("%s in %s", c.LandedByIdentifier, sha)
	case c.LandedByIdentifier != "":
		return c.LandedByIdentifier
	case sha != "":
		return sha
	default:
		return "an unknown commit"
	}
}

func (e *Engine) applyLagLabel(ctx context.Context, t *client.Task, want bool) {
	if t == nil {
		return
	}
	if want {
		_ = e.Board.EnsureLabel(ctx, t, board.LabelLag, "f59e0b")
		return
	}
	_ = e.Board.DropLabel(ctx, t, board.LabelLag)
}

func refOf(t *client.Task) TaskRef {
	return TaskRef{TaskID: t.ID, Identifier: t.Identifier, Title: t.Title}
}
