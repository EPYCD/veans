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

// Package engine is Marshal's brain: every check the CLI, the HTTP service
// and the MCP tools expose is a method here, built on the board snapshot,
// the repository and the checkers.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/marshal/board"
	"code.vikunja.io/veans/internal/marshal/codeowners"
	"code.vikunja.io/veans/internal/marshal/config"
	"code.vikunja.io/veans/internal/marshal/discord"
	"code.vikunja.io/veans/internal/marshal/invariants"
	"code.vikunja.io/veans/internal/marshal/ledger"
	"code.vikunja.io/veans/internal/marshal/notify"
	"code.vikunja.io/veans/internal/marshal/pathpattern"
	"code.vikunja.io/veans/internal/marshal/pool"
	"code.vikunja.io/veans/internal/marshal/refs"
	"code.vikunja.io/veans/internal/marshal/worktree"
	"code.vikunja.io/veans/internal/output"
)

// Engine holds the configuration, the board handle and the state files.
type Engine struct {
	Cfg     *config.Config
	Board   *board.Board
	Ledger  *ledger.Ledger
	Pool    *pool.Store
	Discord *discord.Notifier
	Format  notify.Formatter
	// RepoRoot is the checkout Marshal reads specs and git history from.
	RepoRoot string
	// SpecRev is the revision references resolve against; "" reads the
	// working tree. serve sets it to origin/<default> after fetching.
	SpecRev string

	flags *flagStore

	mu       sync.Mutex
	index    *refs.Index
	corpus   *refs.SpecCorpus
	health   *invariants.Report
	healthAt time.Time
	// lagCache keys each task's measured lag on the branch tip, the base tip
	// and the scope. Lag is a pure function of those three, so a poll where
	// none has moved does no git work at all.
	lagCache map[int64]lagCacheEntry
}

// Load finds .marshal.yml from dir upward, opens the board and the state.
func Load(dir string) (*Engine, error) {
	path, err := config.Find(dir)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return nil, output.Wrap(output.CodeNotConfigured, err, "no .marshal.yml found — run `marshal init` in the repository")
		}
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	b, err := board.Open(cfg.Dir())
	if err != nil {
		return nil, err
	}
	l, err := ledger.Open(cfg.LedgerPath())
	if err != nil {
		return nil, err
	}
	flags, err := openFlags(filepath.Join(cfg.StateDir, "flags.json"))
	if err != nil {
		return nil, err
	}
	e := &Engine{
		Cfg:      cfg,
		Board:    b,
		Ledger:   l,
		Pool:     pool.Open(cfg.PoolPath()),
		Discord:  discord.New(cfg.Discord.WebhookURL, cfg.Discord.Username, cfg.Discord.AvatarURL),
		RepoRoot: cfg.Dir(),
		flags:    flags,
	}
	e.Format = notify.Formatter{TaskURL: b.TaskURL}
	return e, nil
}

// ApplyRelaySettings takes the board's presentation for the relay. Called on
// each poll, so a change in the project's settings applies without a restart.
// The file's values remain the fallback: an empty field on the board means
// "not set here", not "clear what the operator configured".
func (e *Engine) ApplyRelaySettings(s board.RelaySettings) {
	username, avatar := s.Username, s.AvatarURL
	if username == "" {
		username = e.Cfg.Discord.Username
	}
	if avatar == "" {
		avatar = e.Cfg.Discord.AvatarURL
	}
	events := s.Events
	if events == "" {
		events = strings.Join(e.Cfg.Discord.Events, ",")
	}
	e.Discord.SetPresentation(username, avatar, events)
}

// Prefixes lists the configured reference prefixes.
func (e *Engine) Prefixes() []string {
	out := make([]string, 0, len(e.Cfg.References))
	for _, s := range e.Cfg.References {
		out = append(out, s.Prefix)
	}
	return out
}

func (e *Engine) reader() refs.Reader {
	if e.SpecRev == "" {
		return refs.WorktreeReader(e.RepoRoot)
	}
	return refs.GitReader{Root: e.RepoRoot, Rev: e.SpecRev}
}

// Index builds (and caches) the anchor index at the current spec revision.
func (e *Engine) Index(ctx context.Context) (*refs.Index, []string, error) {
	ix, warnings, err := refs.Build(ctx, e.reader(), e.Cfg.References)
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	e.index = ix
	e.mu.Unlock()
	return ix, warnings, nil
}

// Corpus builds (and caches) the paste-detection corpus.
func (e *Engine) Corpus(ctx context.Context) (*refs.SpecCorpus, error) {
	e.mu.Lock()
	c := e.corpus
	e.mu.Unlock()
	if c != nil {
		return c, nil
	}
	c, err := refs.NewSpecCorpus(ctx, e.reader(), e.Cfg.SpecFiles, e.Cfg.PasteMinWords)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.corpus = c
	e.mu.Unlock()
	return c, nil
}

// InvalidateCorpus drops the cached corpus, e.g. after the spec revision moved.
func (e *Engine) InvalidateCorpus() {
	e.mu.Lock()
	e.corpus = nil
	e.mu.Unlock()
}

// TaskReferences is what a task's references render as.
type TaskReferences struct {
	TaskID      int64             `json:"task_id"`
	Identifier  string            `json:"identifier"`
	Resolutions []refs.Resolution `json:"resolutions"`
	Pastes      []refs.PasteMatch `json:"pastes"`
	Broken      int               `json:"broken"`
}

// TaskText is the free text Marshal scans on a task.
func TaskText(t *client.Task) string {
	return t.Title + "\n" + t.Description
}

// ResolveTask resolves the references in a task's title and description at
// read time and runs the paste detector over the same text.
func (e *Engine) ResolveTask(ctx context.Context, t *client.Task) (*TaskReferences, error) {
	ix, _, err := e.Index(ctx)
	if err != nil {
		return nil, err
	}
	found := refs.Extract(TaskText(t), e.Prefixes())
	out := &TaskReferences{TaskID: t.ID, Identifier: t.Identifier, Resolutions: ix.Resolve(found), Pastes: []refs.PasteMatch{}}
	for _, r := range out.Resolutions {
		if !r.Found {
			out.Broken++
		}
	}
	if corpus, err := e.Corpus(ctx); err == nil {
		out.Pastes = corpus.FindPastes(TaskText(t))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// ReferenceReport is the board-level view of M3.3 and M3.4.
type ReferenceReport struct {
	Rev        string            `json:"rev"`
	Tasks      int               `json:"tasks"`
	Referenced int               `json:"referenced"`
	Broken     []BrokenReference `json:"broken"`
	Pastes     []TaskPaste       `json:"pastes"`
	Unlinked   []TaskRef         `json:"unlinked"`
}

// TaskRef names a task in a report.
type TaskRef struct {
	TaskID     int64  `json:"task_id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
}

// BrokenReference is a reference nothing resolves any more.
type BrokenReference struct {
	TaskRef
	Ref string `json:"ref"`
}

// TaskPaste is a verbatim spec run found on a task.
type TaskPaste struct {
	TaskRef
	Match refs.PasteMatch `json:"match"`
}

// References checks every open task's references and text.
func (e *Engine) References(ctx context.Context, snap *board.Snapshot) (*ReferenceReport, error) {
	ix, _, err := e.Index(ctx)
	if err != nil {
		return nil, err
	}
	corpus, err := e.Corpus(ctx)
	if err != nil {
		return nil, err
	}
	rep := &ReferenceReport{Rev: ix.Rev, Tasks: len(snap.Tasks), Broken: []BrokenReference{}, Pastes: []TaskPaste{}, Unlinked: []TaskRef{}}
	prefixes := e.Prefixes()
	for _, t := range snap.Tasks {
		ref := TaskRef{TaskID: t.ID, Identifier: t.Identifier, Title: t.Title}
		found := refs.Extract(TaskText(t), prefixes)
		if len(found) == 0 {
			if len(t.RelatedTasks["subtask"]) == 0 {
				rep.Unlinked = append(rep.Unlinked, ref)
			}
		} else {
			rep.Referenced++
		}
		for _, r := range ix.Resolve(found) {
			if !r.Found {
				rep.Broken = append(rep.Broken, BrokenReference{TaskRef: ref, Ref: r.Ref.ID})
			}
		}
		for _, m := range corpus.FindPastes(TaskText(t)) {
			rep.Pastes = append(rep.Pastes, TaskPaste{TaskRef: ref, Match: m})
		}
	}
	return rep, nil
}

// HealthReport is the invariants report with the snapshot's numbers.
type HealthReport struct {
	invariants.Report
	OpenTasks int       `json:"open_tasks"`
	Leases    int       `json:"leases"`
	CheckedAt time.Time `json:"checked_at"`
}

// Health runs the continuous invariants over a snapshot.
//
// The path checks read the repository rather than the board: the repository is
// the authority on its own shape, and health that agreed with a stale
// published shape would be health agreeing with the bug. A repository whose
// roots cannot be read (no commits yet) simply runs the graph checks, since a
// check that cannot be decided must not be failed.
func (e *Engine) Health(ctx context.Context, snap *board.Snapshot) *HealthReport {
	tasks, leases := snap.InvariantTasks()
	opts := invariants.Options{
		Repository:  e.Board.Cfg.Repository,
		Chokepoints: e.chokepointPatterns(),
	}
	if roots, err := e.RepoRoots(ctx); err == nil {
		opts.Roots = roots
	}
	rep := invariants.Check(tasks, leases, opts)
	h := &HealthReport{Report: rep, OpenTasks: len(snap.Tasks), Leases: len(snap.Leases), CheckedAt: time.Now().UTC()}
	e.mu.Lock()
	e.health = &rep
	e.healthAt = h.CheckedAt
	e.mu.Unlock()
	return h
}

// ChokepointReport is the CODEOWNERS queue. Every rule in the file is a
// chokepoint: they and the claims queued on them share one namespace, the
// repository root, so there is no longer an inside and an outside.
type ChokepointReport struct {
	Source     string             `json:"source"`
	Queues     []codeowners.Queue `json:"queues"`
	Unreadable string             `json:"unreadable,omitempty"`
}

// Chokepoints reads CODEOWNERS and queues every claim on each chokepoint.
func (e *Engine) Chokepoints(snap *board.Snapshot) (*ChokepointReport, error) {
	path := filepath.Join(e.RepoRoot, e.Cfg.Codeowners)
	b, err := os.ReadFile(path) //nolint:gosec // repository file named in the user's config
	if err != nil {
		// An absent CODEOWNERS is a finding for the report, not a failure of it.
		return &ChokepointReport{Source: e.Cfg.Codeowners, Unreadable: err.Error(), Queues: []codeowners.Queue{}}, nil //nolint:nilerr
	}
	f, err := codeowners.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", e.Cfg.Codeowners, err)
	}
	claims := e.claims(snap)
	return &ChokepointReport{Source: e.Cfg.Codeowners, Queues: codeowners.Queues(f.Chokepoints(), claims)}, nil
}

func (e *Engine) claims(snap *board.Snapshot) []codeowners.Claim {
	active := map[int64]map[string]time.Time{}
	for _, l := range snap.Leases {
		if active[l.TaskID] == nil {
			active[l.TaskID] = map[string]time.Time{}
		}
		active[l.TaskID][l.Pattern] = l.Created
	}
	claims := []codeowners.Claim{}
	for _, t := range snap.Tasks {
		assignee := ""
		if len(t.Assignees) > 0 {
			assignee = board.DisplayName(t.Assignees[0])
		}
		blocked := []int64{}
		for _, r := range t.RelatedTasks["blocked"] {
			if r != nil {
				blocked = append(blocked, r.ID)
			}
		}
		patterns := map[string]bool{}
		if t.Scope != nil {
			for _, p := range t.Scope.PathsOwned {
				patterns[p] = true
			}
		}
		for p := range active[t.ID] {
			patterns[p] = true
		}
		for p := range patterns {
			since := t.Created
			isActive := false
			if at, ok := active[t.ID][p]; ok {
				isActive = true
				since = at
			}
			claims = append(claims, codeowners.Claim{
				TaskID: t.ID, Identifier: t.Identifier, Title: t.Title, Assignee: assignee,
				Pattern: p, Since: since, Active: isActive, BlockedBy: blocked,
			})
		}
	}
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].TaskID != claims[j].TaskID {
			return claims[i].TaskID < claims[j].TaskID
		}
		return claims[i].Pattern < claims[j].Pattern
	})
	return claims
}

// OpenOnPath answers "is anything open on this path?": declared scopes and
// live leases overlapping it.
type PathHolder struct {
	TaskRef
	Pattern  string `json:"pattern"`
	Leased   bool   `json:"leased"`
	Assignee string `json:"assignee,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

// CanonicalPath puts a path the caller typed into the form claims are stored
// in, applying the project's repository namespace. Callers that report which
// path they answered about should report this one, not the raw argument: the
// two differ exactly when the answer would otherwise be hard to trust.
func (e *Engine) CanonicalPath(path string) (string, error) {
	norm, err := pathpattern.Canonical(path, e.Board.Cfg.Repository)
	if err != nil {
		return "", output.Wrap(output.CodeValidation, err, "invalid path %q", path)
	}
	return norm, nil
}

// OpenOnPath lists the open tasks whose scope or lease covers path.
func (e *Engine) OpenOnPath(snap *board.Snapshot, path string) ([]PathHolder, error) {
	norm, err := e.CanonicalPath(path)
	if err != nil {
		return nil, err
	}
	leased := map[int64]map[string]bool{}
	for _, l := range snap.Leases {
		if leased[l.TaskID] == nil {
			leased[l.TaskID] = map[string]bool{}
		}
		leased[l.TaskID][l.Pattern] = true
	}
	out := []PathHolder{}
	for _, t := range snap.Tasks {
		if t.Scope == nil {
			continue
		}
		for _, p := range t.Scope.PathsOwned {
			if !pathpattern.Overlap(p, norm) {
				continue
			}
			h := PathHolder{TaskRef: TaskRef{TaskID: t.ID, Identifier: t.Identifier, Title: t.Title}, Pattern: p, Leased: leased[t.ID][p], Branch: board.BranchOf(t)}
			if len(t.Assignees) > 0 {
				h.Assignee = board.DisplayName(t.Assignees[0])
			}
			out = append(out, h)
		}
	}
	return out, nil
}

// BranchCheck is the reconciliation of one branch against its task's claims.
type BranchCheck struct {
	TaskRef
	Branch       string                   `json:"branch"`
	LastActivity time.Time                `json:"last_activity"`
	Stale        bool                     `json:"stale"`
	Changed      int                      `json:"changed_files"`
	Result       *client.ScopeCheckResult `json:"result,omitempty"`
	Error        string                   `json:"error,omitempty"`
}

// Reconcile validates every claimed task's branch against the files it
// actually touched (M1.8) and flags branches with no recent commits (M1.9).
// base is the integration branch to diff against; branch limits the run to
// one branch when set.
func (e *Engine) Reconcile(ctx context.Context, snap *board.Snapshot, base, branch string) ([]BranchCheck, error) {
	out := []BranchCheck{}
	for _, t := range snap.Tasks {
		br := board.BranchOf(t)
		if br == "" || (branch != "" && br != branch) {
			continue
		}
		bc := BranchCheck{TaskRef: TaskRef{TaskID: t.ID, Identifier: t.Identifier, Title: t.Title}, Branch: br}
		last, err := worktree.LastActivity(ctx, e.RepoRoot, br)
		if err != nil {
			bc.Error = err.Error()
			out = append(out, bc)
			continue
		}
		bc.LastActivity = last
		bc.Stale = !last.IsZero() && time.Since(last) > e.Cfg.StaleAfter
		files, err := worktree.ChangedFiles(ctx, e.RepoRoot, base, br)
		if err != nil {
			bc.Error = err.Error()
			out = append(out, bc)
			continue
		}
		bc.Changed = len(files)
		if len(files) > 0 {
			res, err := e.Board.Client.CheckScope(ctx, e.Board.Cfg.ProjectID, &client.ScopeCheckRequest{
				TaskIDs: []int64{t.ID}, Files: pathpattern.CanonicalFiles(files), Repository: e.Board.Cfg.Repository,
			})
			if err != nil {
				bc.Error = err.Error()
			} else {
				bc.Result = res
			}
		}
		out = append(out, bc)
	}
	return out, nil
}

// Reconcile used to strip app_root off git's output before checking it, on the
// theory that board paths were relative to the app directory. They are not:
// the pre-commit hook feeds the same check straight from git, so a claim had
// to be spelled from the repository root to satisfy it — and was then
// unrecognisable here. A whole commit could read "unscoped" for that reason
// alone. Both sides now go through pathpattern.CanonicalFiles.

// WorktreePlan is M4: the commands for a story plus its allocated resources.
type WorktreePlan struct {
	TaskRef
	Plan       worktree.Plan   `json:"plan"`
	Allocation pool.Allocation `json:"allocation"`
	// Forced records that the plan was cut over a refusal, so the JSON says so
	// as plainly as the ledger does.
	Forced bool `json:"forced,omitempty"`
	// Blockers are the open tasks holding a lease on a path this one claims.
	// Present only on a forced plan; a refused one raises them as the error.
	Blockers []PathHolder `json:"blockers,omitempty"`
}

// PlanWorktree derives the branch and directory from the story, allocates a
// database and port to the worker and records the checkout.
//
// The worktree is cut from the integration branch explicitly. Without a start
// point `git worktree add` branches from the invoking checkout's current HEAD,
// so a worker whose clone is a week old starts a week behind in the files they
// are about to edit — the command meant to set them up correctly was the thing
// seeding the staleness.
//
// It refuses when another open task holds a lease on a path this one claims.
// That worker's base is guaranteed to move under exactly these files the
// moment the holder merges, so the queue would be manufacturing the stale
// bases it exists to prevent. This is stronger than the readiness check:
// `veans ready` refuses the claim, but nothing stopped a worker cutting a
// worktree by hand for a task that would become claimable soon.
func (e *Engine) PlanWorktree(ctx context.Context, snap *board.Snapshot, t *client.Task, worker string, force bool) (*WorktreePlan, error) {
	story := worktree.StoryID(t.Title)
	if story == "" {
		story = worktree.StoryID(t.Identifier)
	}
	if story == "" {
		story = strings.ToLower(strings.ReplaceAll(t.Identifier, "-", ""))
		if story == "" {
			story = fmt.Sprintf("t%d", t.ID)
		}
	}

	blockers := leaseBlockers(snap, t)
	if len(blockers) > 0 && !force {
		e.log(ledger.Entry{Action: "allocate", Actor: worker, TaskID: t.ID, Subject: story, Outcome: "refused", Reason: "a claimed path is leased by an open task", Metadata: blockerMetadata(blockers)})
		return nil, output.New(output.CodeConflict, "%s", refusalMessage(t, blockers))
	}

	opts := worktree.BuildOptions{
		Naming: e.Cfg.Worktree, RepoRoot: e.RepoRoot, Story: story,
		Title: t.Title, Identifier: t.Identifier, TaskID: t.ID,
		Base: e.Cfg.IntegrationBranch,
	}
	dry, err := worktree.Build(opts)
	if err != nil {
		return nil, err
	}
	alloc := pool.Allocation{Worker: worker, TaskID: t.ID, Story: story, Branch: dry.Branch, Checkout: dry.Dir, Since: time.Now().UTC()}
	if len(e.Cfg.Pool.Databases) > 0 || len(e.Cfg.Pool.Ports) > 0 {
		alloc, err = e.Pool.Allocate(e.Cfg.Pool, alloc)
		if err != nil {
			e.log(ledger.Entry{Action: "allocate", Actor: worker, TaskID: t.ID, Subject: dry.Dir, Outcome: "refused", Reason: err.Error()})
			return nil, output.Wrap(output.CodeConflict, err, "%v", err)
		}
	}
	opts.Database, opts.Port = alloc.Database, alloc.Port
	plan, err := worktree.Build(opts)
	if err != nil {
		return nil, err
	}
	plan.BaseSHA = e.resolveBase(ctx, plan.Base)
	alloc.Base, alloc.BaseSHA = plan.Base, plan.BaseSHA

	outcome, reason := "ok", ""
	if len(blockers) > 0 {
		outcome, reason = "forced", "cut over a lease held by an open task"
	}
	meta := map[string]any{"branch": plan.Branch, "database": alloc.Database, "port": alloc.Port, "base": plan.Base, "base_sha": plan.BaseSHA}
	if len(blockers) > 0 {
		for k, v := range blockerMetadata(blockers) {
			meta[k] = v
		}
	}
	e.log(ledger.Entry{Action: "allocate", Actor: worker, TaskID: t.ID, Subject: plan.Dir, Outcome: outcome, Reason: reason, Metadata: meta})

	out := &WorktreePlan{TaskRef: TaskRef{TaskID: t.ID, Identifier: t.Identifier, Title: t.Title}, Plan: plan, Allocation: alloc}
	if len(blockers) > 0 {
		out.Forced, out.Blockers = true, blockers
	}
	return out, nil
}

// resolveBase fetches and reads what the integration branch points at now, so
// the recorded sha is the one the worker will actually get rather than the one
// this checkout last saw. Neither step is fatal: a repository with no network
// is still a repository, and refusing to plan a worktree because a fetch timed
// out helps nobody. An unresolved base is recorded as empty rather than wrong.
func (e *Engine) resolveBase(ctx context.Context, base string) string {
	if base == "" {
		return ""
	}
	_ = worktree.Fetch(ctx, e.RepoRoot, worktree.RemoteOf(base))
	sha, err := worktree.ResolveSHA(ctx, e.RepoRoot, base)
	if err != nil {
		return ""
	}
	return sha
}

// leaseBlockers lists the open tasks holding a live lease on a path this task
// claims. A declared-but-unleased claim is not a blocker: nobody is editing
// those files yet, so nothing is about to move under this worker.
func leaseBlockers(snap *board.Snapshot, t *client.Task) []PathHolder {
	if t.Scope == nil || len(t.Scope.PathsOwned) == 0 {
		return nil
	}
	open := map[int64]*client.Task{}
	for _, o := range snap.Tasks {
		open[o.ID] = o
	}
	seen := map[string]bool{}
	out := []PathHolder{}
	for _, l := range snap.Leases {
		if l.TaskID == t.ID {
			continue
		}
		holder := open[l.TaskID]
		if holder == nil {
			// A lease outliving its task is a health finding, not a reason to
			// hold up a worker.
			continue
		}
		for _, mine := range t.Scope.PathsOwned {
			if !pathpattern.Overlap(l.Pattern, mine) {
				continue
			}
			key := fmt.Sprintf("%d|%s", l.TaskID, l.Pattern)
			if seen[key] {
				continue
			}
			seen[key] = true
			h := PathHolder{TaskRef: TaskRef{TaskID: holder.ID, Identifier: holder.Identifier, Title: holder.Title}, Pattern: l.Pattern, Leased: true, Branch: board.BranchOf(holder)}
			if len(holder.Assignees) > 0 {
				h.Assignee = board.DisplayName(holder.Assignees[0])
			}
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskID != out[j].TaskID {
			return out[i].TaskID < out[j].TaskID
		}
		return out[i].Pattern < out[j].Pattern
	})
	return out
}

// maxListedBlockers is how many overlapping paths a refusal spells out. Past a
// handful the list stops being evidence and starts being wallpaper; the count
// carries the rest.
const maxListedBlockers = 4

// refusalMessage names what is held, who holds it, and what unblocks it. A
// refusal that does not say who to wait for is a refusal that gets forced.
func refusalMessage(t *client.Task, blockers []PathHolder) string {
	var b strings.Builder
	me := ref(t.Identifier, t.ID)
	first := blockers[0]

	if len(blockers) == 1 {
		fmt.Fprintf(&b, "%s claims %s, held by %s (in progress). ", me, first.Pattern, ref(first.Identifier, first.TaskID))
		fmt.Fprintf(&b, "A worktree cut now will be stale in that file the moment %s merges. ", ref(first.Identifier, first.TaskID))
	} else {
		fmt.Fprintf(&b, "%s claims %d paths held by open tasks: ", me, len(blockers))
		listed := blockers
		if len(listed) > maxListedBlockers {
			listed = listed[:maxListedBlockers]
		}
		parts := make([]string, 0, len(listed))
		for _, h := range listed {
			parts = append(parts, fmt.Sprintf("%s (%s)", h.Pattern, ref(h.Identifier, h.TaskID)))
		}
		b.WriteString(strings.Join(parts, ", "))
		if rest := len(blockers) - len(listed); rest > 0 {
			fmt.Fprintf(&b, " and %d more", rest)
		}
		b.WriteString(". A worktree cut now will be stale in those files the moment their holders merge. ")
	}

	fmt.Fprintf(&b, "Wait for %s, or re-scope. Override with --force (recorded in the ledger).", holderList(blockers))
	return b.String()
}

// holderList names the distinct tasks to wait for, in the order first seen.
func holderList(blockers []PathHolder) string {
	seen := map[int64]bool{}
	var names []string
	for _, h := range blockers {
		if seen[h.TaskID] {
			continue
		}
		seen[h.TaskID] = true
		names = append(names, ref(h.Identifier, h.TaskID))
	}
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

func ref(identifier string, id int64) string {
	if identifier != "" {
		return identifier
	}
	return fmt.Sprintf("#%d", id)
}

func blockerMetadata(blockers []PathHolder) map[string]any {
	ids := make([]int64, 0, len(blockers))
	paths := make([]string, 0, len(blockers))
	for _, b := range blockers {
		ids = append(ids, b.TaskID)
		paths = append(paths, b.Pattern)
	}
	return map[string]any{"blocked_by": ids, "paths": paths}
}

// Workers is M4.3: who is on which checkout, branch and database, live.
type Workers struct {
	Agents      []*client.ProjectAgent `json:"agents"`
	Allocations []pool.Allocation      `json:"allocations"`
	Worktrees   []worktree.Worktree    `json:"worktrees"`
	Conflicts   []pool.Conflict        `json:"conflicts"`
}

// Workers lists agents, allocations, local worktrees and checkout conflicts.
func (e *Engine) Workers(ctx context.Context) (*Workers, error) {
	agents, err := e.Board.Client.ProjectAgents(ctx, e.Board.Cfg.ProjectID)
	if err != nil {
		return nil, err
	}
	allocs, err := e.Pool.List()
	if err != nil {
		return nil, err
	}
	conflicts, err := e.Pool.Conflicts()
	if err != nil {
		return nil, err
	}
	wts, err := worktree.Existing(ctx, e.RepoRoot)
	if err != nil {
		wts = nil
	}
	if agents == nil {
		agents = []*client.ProjectAgent{}
	}
	if allocs == nil {
		allocs = []pool.Allocation{}
	}
	if conflicts == nil {
		conflicts = []pool.Conflict{}
	}
	if wts == nil {
		wts = []worktree.Worktree{}
	}
	return &Workers{Agents: agents, Allocations: allocs, Worktrees: wts, Conflicts: conflicts}, nil
}

// log appends to the ledger, never failing the caller: the ledger is a
// record, not a gate.
func (e *Engine) log(entry ledger.Entry) {
	if e.Ledger == nil {
		return
	}
	if entry.Actor == "" {
		entry.Actor = e.Board.Identity
	}
	if _, err := e.Ledger.Append(entry); err != nil {
		fmt.Fprintf(os.Stderr, "marshal: ledger: %v\n", err)
	}
}

// Log is the exported form for callers outside the package.
func (e *Engine) Log(entry ledger.Entry) { e.log(entry) }

// Notify posts a Discord message when configured, logging failures. kind is
// the event's name, matched against the board's event filter; pass "" for a
// card that has no name, which is always posted rather than silently dropped.
func (e *Engine) Notify(ctx context.Context, kind string, m *discord.Message) {
	if m == nil || !e.Discord.Enabled() || !e.Discord.Allows(kind) {
		return
	}
	if err := e.Discord.Send(ctx, *m); err != nil {
		fmt.Fprintf(os.Stderr, "marshal: discord: %v\n", err)
	}
}
