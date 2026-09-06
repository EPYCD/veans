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

// Package invariants runs the continuous graph checks over a board export:
// every open task carries a claim, overlapping claims are ordered, the
// blocked graph is acyclic, leases point at live tasks, every stored path is
// canonical, and every chokepoint is reachable from a claim.
package invariants

import (
	"fmt"
	"slices"
	"strings"

	"code.vikunja.io/veans/internal/marshal/pathpattern"
)

// Task is the slice of a board task the checks need.
type Task struct {
	ID         int64
	Identifier string
	Title      string
	Done       bool
	Assignees  []string
	Paths      []string // declared paths_owned
	BlockedBy  []int64  // open or done — the checker filters
	Follows    []int64
	ParentID   int64 // 0 for roots; a parent is a container and is exempt from "carries a claim"
}

// Lease is a live path lease as reported by the board.
type Lease struct {
	TaskID  int64
	Pattern string
	Stale   bool
}

// Finding codes.
const (
	CodeNoClaim          = "no_claim"
	CodeUnorderedOverlap = "unordered_overlap"
	CodeBlockedCycle     = "blocked_cycle"
	CodeLeaseWithoutTask = "lease_without_task"
	CodeStaleLease       = "stale_lease"
	CodeParentWithPaths  = "parent_with_paths"
	// CodeNonCanonicalPath is a stored claim that is not in the canonical
	// form: either spelled differently from how it would be stored today, or
	// written against a base that is not the repository root. Either way it is
	// a second identity waiting for the file it names.
	CodeNonCanonicalPath = "non_canonical_path"
	// CodeUnreachableChokepoint is a chokepoint that no claim could ever
	// overlap because it is not in the namespace claims are stored in. Its
	// queue is not quiet, it is unanswerable — and an empty queue and a
	// healthy one render identically, which is how the first one shipped and
	// stayed shipped.
	CodeUnreachableChokepoint = "unreachable_chokepoint"
)

// Options carries what the path checks need and the graph checks do not: the
// repository namespace, the repository's own top-level entries, and the
// chokepoints read out of CODEOWNERS. The zero value disables both path
// checks, which is what a repository that has published no roots gets.
type Options struct {
	// Repository is .veans.yml's repository, the `repo:` namespace.
	Repository string
	// Roots is what a repository-root-relative path looks like here.
	Roots pathpattern.Roots
	// Chokepoints are the CODEOWNERS-derived patterns, already canonical.
	Chokepoints []string
}

// Finding is one violated or warned invariant.
// The json tags here and on Report are load-bearing: both cross the wire to
// the board's health panel, which reads snake_case. Without them Go emits its
// own field names and the panel reads undefined off every field.
type Finding struct {
	Code    string   `json:"code"` // one of the Code* constants
	Message string   `json:"message"`
	TaskIDs []int64  `json:"task_ids"`
	Paths   []string `json:"paths"`
}

// Report is the outcome of one Check run.
type Report struct {
	Tasks          int       `json:"tasks"`      // open, non-container tasks considered
	Containers     int       `json:"containers"` // tasks that have children
	Collisions     int       `json:"collisions"` // unordered overlaps
	Cycles         int       `json:"cycles"`
	UnblockedRoots []int64   `json:"unblocked_roots"` // open tasks with no open BlockedBy/Follows and no open subtasks (ids, ascending)
	Findings       []Finding `json:"findings"`
	OK             bool      `json:"ok"` // no findings with code no_claim, unordered_overlap or blocked_cycle
}

type graph struct {
	open       map[int64]*Task
	containers map[int64]bool
	// order edges: BlockedBy ∪ Follows ∪ parent, restricted to open tasks.
	order map[int64][]int64
	// cycle edges: BlockedBy ∪ Follows only — a parent link is not a cycle.
	blocked map[int64][]int64
	ids     []int64 // open task ids, ascending
}

// Check evaluates the invariants over the given tasks and leases. Only open
// tasks (Done == false) are considered; done tasks matter solely as the
// targets of dangling leases.
func Check(tasks []Task, leases []Lease, opts Options) Report {
	g := build(tasks)
	var findings []Finding
	findings = append(findings, claimFindings(g)...)
	overlaps := overlapFindings(g)
	findings = append(findings, overlaps...)
	cycles := cycleFindings(g)
	findings = append(findings, cycles...)
	findings = append(findings, leaseFindings(g, leases)...)
	findings = append(findings, canonicalFindings(g, leases, opts)...)
	findings = append(findings, chokepointFindings(opts)...)

	slices.SortFunc(findings, compareFindings)

	r := Report{
		Containers:     len(g.containers),
		Tasks:          len(g.open) - len(g.containers),
		Collisions:     len(overlaps),
		Cycles:         len(cycles),
		UnblockedRoots: unblockedRoots(g),
		Findings:       findings,
		OK:             true,
	}
	for _, f := range findings {
		switch f.Code {
		case CodeNoClaim, CodeUnorderedOverlap, CodeBlockedCycle,
			CodeNonCanonicalPath, CodeUnreachableChokepoint:
			r.OK = false
		}
	}
	return r
}

// canonicalFindings is the path-canonicality invariant: no stored pattern —
// declared scope or live lease — differs from its canonical form.
//
// It catches a regression in any producer, including one added later. Every
// producer of a scope path is supposed to go through pathpattern.Canonical
// now, but "supposed to" is not an assertion, and the cost of one that slips
// through is a lease that silently protects nothing.
func canonicalFindings(g *graph, leases []Lease, opts Options) []Finding {
	var out []Finding
	check := func(pattern string, taskID int64) {
		canonical, err := pathpattern.Canonical(pattern, opts.Repository)
		switch {
		case err != nil:
			out = append(out, Finding{
				Code:    CodeNonCanonicalPath,
				Message: fmt.Sprintf("%s claims %q, which is not a valid scope path: %v", g.label(taskID), pattern, err),
				TaskIDs: []int64{taskID},
				Paths:   []string{pattern},
			})
		case canonical != pattern:
			out = append(out, Finding{
				Code:    CodeNonCanonicalPath,
				Message: fmt.Sprintf("%s claims %q, which is stored as %q — two spellings of one file are two claims", g.label(taskID), pattern, canonical),
				TaskIDs: []int64{taskID},
				Paths:   []string{pattern},
			})
		default:
			if err := opts.Roots.Check(canonical); err != nil {
				out = append(out, Finding{
					Code:    CodeNonCanonicalPath,
					Message: fmt.Sprintf("%s: %v", g.label(taskID), err),
					TaskIDs: []int64{taskID},
					Paths:   []string{pattern},
				})
			}
		}
	}
	for _, id := range g.ids {
		for _, p := range g.open[id].Paths {
			check(p, id)
		}
	}
	for _, l := range leases {
		// A lease outliving its task is already its own finding; checking its
		// pattern too would just say the same thing twice.
		if g.open[l.TaskID] != nil {
			check(l.Pattern, l.TaskID)
		}
	}
	return out
}

// chokepointFindings is the chokepoint-reachability invariant: every
// CODEOWNERS-derived pattern must live in the same namespace as a stored
// claim, so that the queue for it is capable of having a row in it.
//
// This is the invariant whose absence let an always-empty queue ship and stay
// shipped for the life of the feature. A queue with nothing in it looks
// exactly like a queue with nothing to report, so no amount of looking at the
// output would have found it; only asking whether the question was answerable
// does.
func chokepointFindings(opts Options) []Finding {
	if !opts.Roots.Declared() {
		return nil
	}
	var out []Finding
	for _, cp := range opts.Chokepoints {
		canonical, err := pathpattern.Canonical(cp, opts.Repository)
		if err != nil {
			out = append(out, Finding{
				Code:    CodeUnreachableChokepoint,
				Message: fmt.Sprintf("chokepoint %q is not a valid scope path: %v — nothing can ever queue on it", cp, err),
				Paths:   []string{cp},
			})
			continue
		}
		if err := opts.Roots.Check(canonical); err != nil {
			out = append(out, Finding{
				Code:    CodeUnreachableChokepoint,
				Message: fmt.Sprintf("chokepoint %q is not in the namespace claims are stored in, so its queue can never be non-empty: %v", cp, err),
				Paths:   []string{cp},
			})
		}
	}
	return out
}

func build(tasks []Task) *graph {
	g := &graph{
		open:       map[int64]*Task{},
		containers: map[int64]bool{},
		order:      map[int64][]int64{},
		blocked:    map[int64][]int64{},
	}
	for i := range tasks {
		t := &tasks[i]
		if !t.Done {
			g.open[t.ID] = t
			g.ids = append(g.ids, t.ID)
		}
	}
	slices.Sort(g.ids)
	// A container is any open task some task — open or done — hangs under:
	// an epic whose children all shipped is still a container, not a leaf
	// that forgot its claim.
	for i := range tasks {
		if p := tasks[i].ParentID; p != 0 && g.open[p] != nil {
			g.containers[p] = true
		}
	}
	for _, id := range g.ids {
		t := g.open[id]
		for _, b := range t.BlockedBy {
			g.addEdge(id, b, true)
		}
		for _, f := range t.Follows {
			g.addEdge(id, f, true)
		}
		if t.ParentID != 0 {
			g.addEdge(id, t.ParentID, false)
		}
	}
	return g
}

func (g *graph) addEdge(from, to int64, blocking bool) {
	if from == to || g.open[to] == nil {
		return
	}
	g.order[from] = append(g.order[from], to)
	if blocking {
		g.blocked[from] = append(g.blocked[from], to)
	}
}

func (g *graph) label(id int64) string {
	if t := g.open[id]; t != nil && t.Identifier != "" {
		return t.Identifier
	}
	return fmt.Sprintf("#%d", id)
}

func claimFindings(g *graph) []Finding {
	var out []Finding
	for _, id := range g.ids {
		t := g.open[id]
		switch {
		case g.containers[id] && len(t.Paths) > 0:
			out = append(out, Finding{
				Code:    CodeParentWithPaths,
				Message: fmt.Sprintf("%s is a container but declares paths; move the claim to its subtasks", g.label(id)),
				TaskIDs: []int64{id},
				Paths:   slices.Clone(t.Paths),
			})
		case !g.containers[id] && len(t.Paths) == 0:
			out = append(out, Finding{
				Code:    CodeNoClaim,
				Message: fmt.Sprintf("%s is open and declares no paths_owned", g.label(id)),
				TaskIDs: []int64{id},
			})
		}
	}
	return out
}

func overlapFindings(g *graph) []Finding {
	reach := g.closure()
	var out []Finding
	for i, a := range g.ids {
		for _, b := range g.ids[i+1:] {
			if reach[a][b] || reach[b][a] {
				continue
			}
			paths := overlappingPaths(g.open[a].Paths, g.open[b].Paths)
			if len(paths) == 0 {
				continue
			}
			out = append(out, Finding{
				Code: CodeUnorderedOverlap,
				Message: fmt.Sprintf("%s and %s both claim %s with no blocked-by/follows ordering between them",
					g.label(a), g.label(b), strings.Join(paths, ", ")),
				TaskIDs: []int64{a, b},
				Paths:   paths,
			})
		}
	}
	return out
}

// closure returns, per open task, the set of open tasks it reaches through
// order edges.
func (g *graph) closure() map[int64]map[int64]bool {
	reach := make(map[int64]map[int64]bool, len(g.ids))
	for _, id := range g.ids {
		seen := map[int64]bool{}
		stack := slices.Clone(g.order[id])
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, g.order[n]...)
		}
		reach[id] = seen
	}
	return reach
}

func overlappingPaths(as, bs []string) []string {
	var out []string
	for _, a := range as {
		for _, b := range bs {
			if pathpattern.Overlap(a, b) {
				out = appendUnique(out, a, b)
			}
		}
	}
	return out
}

func appendUnique(list []string, items ...string) []string {
	for _, it := range items {
		if !slices.Contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}

// cycleFindings reports each strongly connected component of the blocked
// graph with more than one member (or a self-loop) once.
func cycleFindings(g *graph) []Finding {
	var out []Finding
	for _, members := range stronglyConnected(g.ids, g.blocked) {
		if len(members) < 2 {
			continue
		}
		slices.Sort(members)
		labels := make([]string, len(members))
		for i, id := range members {
			labels[i] = g.label(id)
		}
		out = append(out, Finding{
			Code:    CodeBlockedCycle,
			Message: fmt.Sprintf("%s block each other in a cycle; none of them can ever become ready", strings.Join(labels, ", ")),
			TaskIDs: members,
		})
	}
	return out
}

// stronglyConnected is Tarjan's algorithm; components come back in the order
// they complete, members in discovery order.
func stronglyConnected(ids []int64, edges map[int64][]int64) [][]int64 {
	var (
		index    int
		indices  = map[int64]int{}
		lowlink  = map[int64]int{}
		onStack  = map[int64]bool{}
		stack    []int64
		result   [][]int64
		strongly func(v int64)
	)
	strongly = func(v int64) {
		indices[v] = index
		lowlink[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range edges[v] {
			if _, visited := indices[w]; !visited {
				strongly(w)
				lowlink[v] = min(lowlink[v], lowlink[w])
			} else if onStack[w] {
				lowlink[v] = min(lowlink[v], indices[w])
			}
		}
		if lowlink[v] != indices[v] {
			return
		}
		var comp []int64
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			comp = append(comp, w)
			if w == v {
				break
			}
		}
		result = append(result, comp)
	}
	for _, id := range ids {
		if _, visited := indices[id]; !visited {
			strongly(id)
		}
	}
	return result
}

func leaseFindings(g *graph, leases []Lease) []Finding {
	var out []Finding
	for _, l := range leases {
		if g.open[l.TaskID] == nil {
			out = append(out, Finding{
				Code:    CodeLeaseWithoutTask,
				Message: fmt.Sprintf("lease on %s is held by #%d, which is not an open task", l.Pattern, l.TaskID),
				TaskIDs: []int64{l.TaskID},
				Paths:   []string{l.Pattern},
			})
		}
		if l.Stale {
			out = append(out, Finding{
				Code:    CodeStaleLease,
				Message: fmt.Sprintf("lease on %s held by %s is stale", l.Pattern, g.label(l.TaskID)),
				TaskIDs: []int64{l.TaskID},
				Paths:   []string{l.Pattern},
			})
		}
	}
	return out
}

func unblockedRoots(g *graph) []int64 {
	var out []int64
	for _, id := range g.ids {
		if g.containers[id] || len(g.blocked[id]) > 0 {
			continue
		}
		out = append(out, id)
	}
	return out
}

func compareFindings(a, b Finding) int {
	if c := strings.Compare(a.Code, b.Code); c != 0 {
		return c
	}
	if c := slices.Compare(a.TaskIDs, b.TaskIDs); c != 0 {
		return c
	}
	return slices.Compare(a.Paths, b.Paths)
}
