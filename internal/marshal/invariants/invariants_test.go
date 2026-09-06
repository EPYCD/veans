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

package invariants

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"code.vikunja.io/veans/internal/marshal/pathpattern"
)

// board mirrors the brief: two ordered overlaps, one unordered, a blocked
// cycle, an epic with two children, and a lease left on a done task.
func board() ([]Task, []Lease) {
	tasks := []Task{
		{ID: 1, Identifier: "CY-1", Title: "Contract: hire endpoint", Paths: []string{"src/lib/contract.ts"}},
		{ID: 2, Identifier: "CY-2", Title: "Contract: fire endpoint", Paths: []string{"src/lib/contract.ts"}, BlockedBy: []int64{1}},
		{ID: 3, Identifier: "CY-3", Title: "Schema: add field", Paths: []string{"src/server/db/schema.ts"}},
		{ID: 4, Identifier: "CY-4", Title: "Schema: add index", Paths: []string{"src/server/db/**"}},
		{ID: 5, Identifier: "CY-5", Title: "Engine A", Paths: []string{"packages/engine/a.ts"}, BlockedBy: []int64{6}},
		{ID: 6, Identifier: "CY-6", Title: "Engine B", Paths: []string{"packages/engine/b.ts"}, BlockedBy: []int64{5}},
		{ID: 7, Identifier: "CY-7", Title: "Epic: auth"},
		{ID: 8, Identifier: "CY-8", Title: "Auth: session", ParentID: 7, Paths: []string{"src/server/auth/session.ts"}},
		{ID: 9, Identifier: "CY-9", Title: "Auth: not scoped yet", ParentID: 7},
		{ID: 10, Identifier: "CY-10", Title: "Shipped", Done: true, Paths: []string{"src/lib/contract.ts"}},
	}
	leases := []Lease{
		{TaskID: 1, Pattern: "src/lib/contract.ts"},
		{TaskID: 3, Pattern: "src/server/db/schema.ts", Stale: true},
		{TaskID: 10, Pattern: "src/lib/contract.ts"},
	}
	return tasks, leases
}

func summarize(findings []Finding) []string {
	out := make([]string, len(findings))
	for i, f := range findings {
		out[i] = fmt.Sprintf("%s %v %v", f.Code, f.TaskIDs, f.Paths)
	}
	return out
}

func TestCheckBoard(t *testing.T) {
	tasks, leases := board()
	r := Check(tasks, leases, Options{})

	if r.Tasks != 8 || r.Containers != 1 || r.Collisions != 1 || r.Cycles != 1 || r.OK {
		t.Errorf("counts: tasks=%d containers=%d collisions=%d cycles=%d ok=%v", r.Tasks, r.Containers, r.Collisions, r.Cycles, r.OK)
	}
	if want := []int64{1, 3, 4, 8, 9}; !reflect.DeepEqual(r.UnblockedRoots, want) {
		t.Errorf("roots = %v, want %v", r.UnblockedRoots, want)
	}
	want := []string{
		"blocked_cycle [5 6] []",
		"lease_without_task [10] [src/lib/contract.ts]",
		"no_claim [9] []",
		"stale_lease [3] [src/server/db/schema.ts]",
		"unordered_overlap [3 4] [src/server/db/schema.ts src/server/db/**]",
	}
	if got := summarize(r.Findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q, want %q", got, want)
	}
	for _, f := range r.Findings {
		if f.Message == "" {
			t.Errorf("%s has no message", f.Code)
		}
	}
	if msg := r.Findings[4].Message; !strings.Contains(msg, "CY-3") || !strings.Contains(msg, "CY-4") {
		t.Errorf("overlap message should name both tasks: %q", msg)
	}
}

func TestCheckIsDeterministic(t *testing.T) {
	tasks, leases := board()
	first := Check(tasks, leases, Options{})
	slices.Reverse(tasks)
	slices.Reverse(leases)
	second := Check(tasks, leases, Options{})
	if !reflect.DeepEqual(first, second) {
		t.Errorf("order-dependent report:\n%+v\n%+v", first, second)
	}
}

// TestCheckPathCanonicality is the path-canonicality invariant: no stored
// pattern may differ from its canonical form, and none may be written against
// a base that is not the repository root. It catches a regression in any
// producer, including one added after this was written — "every producer goes
// through the one function" is a convention until something asserts it.
func TestCheckPathCanonicality(t *testing.T) {
	opts := Options{Roots: pathpattern.ParseRoots("app,docs,.github", "app", "src,packages")}
	tasks := []Task{
		{ID: 1, Paths: []string{"app/src/x.ts"}},
		// Spelled differently from how it would be stored today.
		{ID: 2, Paths: []string{"./app/src//y.ts"}},
		// Canonically spelled, but relative to the application rather than the
		// repository — the shape that put two live leases on one file.
		{ID: 3, Paths: []string{"src/z.ts"}},
		// Anchored nowhere, so there is no base to be wrong about.
		{ID: 4, Paths: []string{"**/*.md"}},
	}
	r := Check(tasks, nil, opts)

	got := summarize(r.Findings)
	want := []string{
		"non_canonical_path [2] [./app/src//y.ts]",
		"non_canonical_path [3] [src/z.ts]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q, want %q", got, want)
	}
	if r.OK {
		t.Error("a claim that is not canonical must fail the report, not warn")
	}
	for _, f := range r.Findings {
		if f.TaskIDs[0] == 3 && !strings.Contains(f.Message, `"app/src/z.ts"`) {
			t.Errorf("the finding for a wrong base should name the spelling meant: %s", f.Message)
		}
	}

	// A live lease is checked too: it is the thing exclusion is actually
	// enforced on, so a lease nobody could collide with is the worst version
	// of this bug.
	r = Check([]Task{{ID: 1, Paths: []string{"app/a.ts"}}}, []Lease{{TaskID: 1, Pattern: "src/a.ts"}}, opts)
	if len(r.Findings) != 1 || r.Findings[0].Code != CodeNonCanonicalPath {
		t.Errorf("a lease on a non-canonical path must be a finding: %q", summarize(r.Findings))
	}

	// A repository that has published no roots cannot have this decided for
	// it, and a check that cannot be decided must not be failed.
	r = Check(tasks, nil, Options{})
	for _, f := range r.Findings {
		if f.Code == CodeNonCanonicalPath && f.TaskIDs[0] == 3 {
			t.Error("without declared roots there is no way to know which base was meant")
		}
	}
}

// TestCheckChokepointReachability is the invariant whose absence let an
// always-empty queue ship and stay shipped for the life of the feature. A
// queue with nothing in it looks exactly like a queue with nothing to report,
// so no amount of reading the output finds this; only asking whether the
// question is answerable does.
func TestCheckChokepointReachability(t *testing.T) {
	opts := Options{
		Roots: pathpattern.ParseRoots("app,docs,.github", "app", "src,packages"),
		// What CODEOWNERS ingestion used to produce on a project with an
		// app_root: the anchoring root stripped off, leaving a pattern in a
		// namespace no claim is ever stored in.
		Chokepoints: []string{"src/server/db/repo.ts", "app/src/lib/contract.ts", ".github/**"},
	}
	tasks := []Task{{ID: 1, Paths: []string{"app/src/server/db/repo.ts"}}}
	r := Check(tasks, nil, opts)

	got := summarize(r.Findings)
	want := []string{"unreachable_chokepoint [] [src/server/db/repo.ts]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q, want %q", got, want)
	}
	if r.OK {
		t.Error("a chokepoint nothing can queue on must fail the report")
	}

	// The fixed ingestion keeps every chokepoint in the claims' namespace.
	opts.Chokepoints = []string{"app/src/server/db/repo.ts", "app/src/lib/contract.ts", ".github/**"}
	if r := Check(tasks, nil, opts); !r.OK || len(r.Findings) != 0 {
		t.Errorf("canonical chokepoints must be clean: %q", summarize(r.Findings))
	}
}

func TestCheckCleanBoard(t *testing.T) {
	tasks := []Task{
		{ID: 1, Paths: []string{"src/lib/contract.ts"}},
		{ID: 2, Paths: []string{"src/lib/**"}, Follows: []int64{1}},
		{ID: 3, Paths: []string{"src/lib/contract.ts"}, BlockedBy: []int64{2}},
		// A blocker that already shipped no longer holds anything back.
		{ID: 4, Paths: []string{"docs/**"}, BlockedBy: []int64{5}},
		{ID: 5, Done: true, Paths: []string{"docs/**"}},
	}
	r := Check(tasks, nil, Options{})
	if !r.OK || len(r.Findings) != 0 {
		t.Errorf("clean board reported %q", summarize(r.Findings))
	}
	if want := []int64{1, 4}; !reflect.DeepEqual(r.UnblockedRoots, want) {
		t.Errorf("roots = %v, want %v", r.UnblockedRoots, want)
	}
	if r.Tasks != 4 || r.Containers != 0 {
		t.Errorf("tasks=%d containers=%d", r.Tasks, r.Containers)
	}
}

func TestCheckParentWithPaths(t *testing.T) {
	tasks := []Task{
		{ID: 1, Identifier: "EPIC", Paths: []string{"src/server/auth/**"}},
		{ID: 2, ParentID: 1, Paths: []string{"src/server/auth/session.ts"}},
		{ID: 3, ParentID: 1, Paths: []string{"src/server/auth/login.ts"}},
		// Siblings are not ordered by sharing a parent.
		{ID: 4, ParentID: 1, Paths: []string{"src/server/auth/login.ts"}},
	}
	r := Check(tasks, nil, Options{})
	want := []string{
		"parent_with_paths [1] [src/server/auth/**]",
		"unordered_overlap [3 4] [src/server/auth/login.ts]",
	}
	if got := summarize(r.Findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q, want %q", got, want)
	}
	if r.OK {
		t.Error("an unordered sibling overlap must fail the report")
	}
	if r.Containers != 1 || r.Tasks != 3 {
		t.Errorf("tasks=%d containers=%d", r.Tasks, r.Containers)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(r.UnblockedRoots, want) {
		t.Errorf("roots = %v", r.UnblockedRoots)
	}
}

func TestCheckWarningsKeepOK(t *testing.T) {
	tasks := []Task{
		{ID: 1, Paths: []string{"a/**"}},
		{ID: 2, ParentID: 1, Paths: []string{"a/b.ts"}},
		{ID: 3, Done: true},
	}
	leases := []Lease{{TaskID: 1, Pattern: "a/**", Stale: true}, {TaskID: 3, Pattern: "x"}}
	r := Check(tasks, leases, Options{})
	if !r.OK {
		t.Errorf("warnings alone must not fail the report: %q", summarize(r.Findings))
	}
	if len(r.Findings) != 3 {
		t.Errorf("findings = %q", summarize(r.Findings))
	}
}

func TestCheckCycleThroughFollows(t *testing.T) {
	tasks := []Task{
		{ID: 1, Paths: []string{"a"}, Follows: []int64{2}},
		{ID: 2, Paths: []string{"b"}, BlockedBy: []int64{3}},
		{ID: 3, Paths: []string{"c"}, Follows: []int64{1}},
		{ID: 4, Paths: []string{"d"}, BlockedBy: []int64{4}}, // self-loop is not a cycle finding
		{ID: 5, Paths: []string{"e"}, BlockedBy: []int64{6}},
		{ID: 6, Paths: []string{"f"}, BlockedBy: []int64{5}},
	}
	r := Check(tasks, nil, Options{})
	want := []string{"blocked_cycle [1 2 3] []", "blocked_cycle [5 6] []"}
	if got := summarize(r.Findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q, want %q", got, want)
	}
	if r.Cycles != 2 || len(r.UnblockedRoots) != 1 || r.UnblockedRoots[0] != 4 {
		t.Errorf("cycles=%d roots=%v", r.Cycles, r.UnblockedRoots)
	}
}

func TestCheckEmpty(t *testing.T) {
	r := Check(nil, nil, Options{})
	if !r.OK || r.Tasks != 0 || len(r.Findings) != 0 || r.UnblockedRoots != nil {
		t.Errorf("empty report = %+v", r)
	}
}
