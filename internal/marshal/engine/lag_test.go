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
	"path/filepath"
	"testing"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/marshal/board"
)

// TestSeverityFor is the severity ladder, which is what makes lag worth
// reading. A tool that says "you are behind" is ignored within a week; one
// that says "#43 landed schema.ts, you are editing schema.ts, you will
// conflict" is obeyed, because that is a specific and checkable claim.
func TestSeverityFor(t *testing.T) {
	scope := &client.TaskScope{
		PathsOwned:    []string{"app/src/server/db/schema.ts", "app/packages/engine/**"},
		PathsAffected: []string{"app/src/lib/contract/**", "docs/API.md"},
	}
	cases := map[string]string{
		// Inside paths_owned: a textual conflict is certain, not likely.
		"app/src/server/db/schema.ts":          client.LagSeverityOwned,
		"app/packages/engine/src/agreement.ts": client.LagSeverityOwned,
		// Inside paths_affected: no conflict, but the code you depend on moved
		// and your gates will not see it until you rebase. This is what
		// paths_affected is finally for — until now it was recorded and never
		// read.
		"app/src/lib/contract/index.ts": client.LagSeverityAffected,
		"docs/API.md":                   client.LagSeverityAffected,
		// Outside the scope entirely: counted, never gating.
		"app/src/pages/home.tsx":  client.LagSeverityElsewhere,
		".github/workflows/x.yml": client.LagSeverityElsewhere,
	}
	for file, want := range cases {
		if got := severityFor(scope, file, ""); got != want {
			t.Errorf("severityFor(%q) = %q, want %q", file, got, want)
		}
	}

	// A file both owned and affected is owned: a certain conflict outranks a
	// stale assumption.
	both := &client.TaskScope{
		PathsOwned:    []string{"app/src/x.ts"},
		PathsAffected: []string{"app/src/**"},
	}
	if got := severityFor(both, "app/src/x.ts", ""); got != client.LagSeverityOwned {
		t.Errorf("owned must win over affected, got %q", got)
	}

	if got := severityFor(nil, "anything", ""); got != client.LagSeverityElsewhere {
		t.Errorf("a task with no scope holds nothing: %q", got)
	}
}

// TestSeverityForNamespacedScope: in a multi-repo project the claim carries a
// repo: prefix while git prints a bare path. Comparing across those two
// dialects would report every file as elsewhere and quietly find no lag at all.
func TestSeverityForNamespacedScope(t *testing.T) {
	scope := &client.TaskScope{PathsOwned: []string{"api:pkg/models/tasks.go"}}
	if got := severityFor(scope, "pkg/models/tasks.go", "api"); got != client.LagSeverityOwned {
		t.Errorf("severityFor with repository namespace = %q, want owned", got)
	}
	// A file in a different repository is not this task's problem.
	if got := severityFor(scope, "pkg/models/tasks.go", "web"); got != client.LagSeverityElsewhere {
		t.Errorf("a path in another repository = %q, want elsewhere", got)
	}
}

// TestMaxLagSeverity: the top-level severity is the worst across collisions,
// so a consumer can gate on one field.
func TestMaxLagSeverity(t *testing.T) {
	if got := client.MaxLagSeverity(nil); got != "" {
		t.Errorf("no collisions = %q, want empty", got)
	}
	mixed := []*client.LagCollision{
		{Severity: client.LagSeverityElsewhere},
		{Severity: client.LagSeverityOwned},
		{Severity: client.LagSeverityAffected},
	}
	if got := client.MaxLagSeverity(mixed); got != client.LagSeverityOwned {
		t.Errorf("MaxLagSeverity = %q, want owned", got)
	}
	if got := client.MaxLagSeverity(mixed[:1]); got != client.LagSeverityElsewhere {
		t.Errorf("MaxLagSeverity = %q, want elsewhere", got)
	}
}

// TestBlockingOnlyOnOwned pins the rule the whole ladder rests on. Gating on
// affected would make overriding routine, and a --force everyone types by
// reflex defeats the gate on owned too.
func TestBlockingOnlyOnOwned(t *testing.T) {
	for sev, want := range map[string]bool{
		client.LagSeverityOwned:     true,
		client.LagSeverityAffected:  false,
		client.LagSeverityElsewhere: false,
		"":                          false,
	} {
		if got := (&client.TaskLag{Severity: sev}).Blocking(); got != want {
			t.Errorf("severity %q blocking = %v, want %v", sev, got, want)
		}
	}
	var nilLag *client.TaskLag
	if nilLag.Blocking() {
		t.Error("no lag record blocks nothing")
	}
}

// TestScopeRevision: widening a scope changes what counts as a collision
// without moving either sha, so it has to be part of the cache key or the
// answer goes stale exactly when someone has just made it more important.
func TestScopeRevision(t *testing.T) {
	a := &client.Task{Scope: &client.TaskScope{PathsOwned: []string{"a.ts"}}}
	b := &client.Task{Scope: &client.TaskScope{PathsOwned: []string{"a.ts", "b.ts"}}}
	if scopeRevision(a) == scopeRevision(b) {
		t.Error("a widened scope must change the cache key")
	}
	// paths_affected counts too: it decides `affected` collisions.
	c := &client.Task{Scope: &client.TaskScope{PathsOwned: []string{"a.ts"}, PathsAffected: []string{"z.ts"}}}
	if scopeRevision(a) == scopeRevision(c) {
		t.Error("paths_affected is part of the answer, so part of the key")
	}
	if scopeRevision(&client.Task{}) != "" {
		t.Error("no scope, no revision")
	}
}

// TestLagCardFiresOnTransitionIntoOwnedOnly is the notification rule. A
// channel that fires on every poll is a channel everyone mutes, and the owned
// card is then lost along with it — so the only moment worth interrupting
// someone for is the one where a conflict stopped being hypothetical.
//
// It exercises the flag store directly, which is what announceLag gates on:
// Seen records the new severity and reports whether it was already that, so a
// card fires exactly on a change into owned.
func TestLagCardFiresOnTransitionIntoOwnedOnly(t *testing.T) {
	dir := t.TempDir()
	flags, err := openFlags(filepath.Join(dir, "flags.json"))
	if err != nil {
		t.Fatal(err)
	}

	// wouldCard mirrors announceLag's decision, without a Discord client.
	wouldCard := func(severity string) bool {
		changed := !flags.Seen(lagFlagKey(51), severity)
		return changed && severity == client.LagSeverityOwned
	}

	if wouldCard(client.LagSeverityElsewhere) {
		t.Error("elsewhere must never card")
	}
	if wouldCard(client.LagSeverityAffected) {
		t.Error("affected must never card")
	}
	if !wouldCard(client.LagSeverityOwned) {
		t.Error("crossing into owned must card")
	}
	// Still owned on the next poll, and the one after: nothing has changed,
	// so nothing is said.
	if wouldCard(client.LagSeverityOwned) {
		t.Error("a card must not repeat while the severity is unchanged")
	}
	if wouldCard(client.LagSeverityOwned) {
		t.Error("still no")
	}
	// Dropping to affected is not a card either.
	if wouldCard(client.LagSeverityAffected) {
		t.Error("falling back to affected must not card")
	}
	// Climbing back into owned is a new transition, and is.
	if !wouldCard(client.LagSeverityOwned) {
		t.Error("crossing into owned again must card again")
	}

	// A branch that catches up has its record cleared, and the remembered
	// severity with it — otherwise falling behind again would be silent.
	flags.Clear(lagFlagKey(51))
	if !wouldCard(client.LagSeverityOwned) {
		t.Error("after catching up, falling behind again must card")
	}
}

// TestSnapshotByRef: a Refs: trailer names a project index, not a task id.
// Matching it against ids would attribute a change to whatever task happened
// to hold that id, which is worse than leaving it unattributed.
func TestSnapshotByRef(t *testing.T) {
	snap := &board.Snapshot{
		Tasks:     []*client.Task{{ID: 900, Index: 51, Identifier: "CY-51"}},
		DoneTasks: []*client.Task{{ID: 700, Index: 43, Identifier: "CY-43"}},
	}
	for _, ref := range []string{"CY-43", "cy-43", "#43", "43"} {
		got := snap.ByRef(ref)
		if got == nil || got.ID != 700 {
			t.Errorf("ByRef(%q) = %v, want the done task 700", ref, got)
		}
	}
	if got := snap.ByRef("CY-51"); got == nil || got.ID != 900 {
		t.Errorf("ByRef of an open task = %v", got)
	}
	// 900 is a task id, not an index — nothing should match it.
	if got := snap.ByRef("900"); got != nil {
		t.Errorf("ByRef(900) matched on id, not index: %v", got)
	}
	if got := snap.ByRef("nope"); got != nil {
		t.Errorf("ByRef(nope) = %v", got)
	}
}

// TestResolveLanded: a commit with no Refs: trailer keeps its sha and gains no
// task. A hand-pushed commit is still lag.
func TestResolveLanded(t *testing.T) {
	snap := &board.Snapshot{DoneTasks: []*client.Task{{ID: 700, Index: 43, Identifier: "CY-43"}}}

	c := &client.LagCollision{LandedInSHA: "141e6cd"}
	resolveLanded(snap, c, []string{"CY-43"})
	if c.LandedByTaskID != 700 || c.LandedByIdentifier != "CY-43" {
		t.Errorf("collision = %+v", c)
	}

	unattributed := &client.LagCollision{LandedInSHA: "deadbee"}
	resolveLanded(snap, unattributed, nil)
	if unattributed.LandedByTaskID != 0 || unattributed.LandedInSHA != "deadbee" {
		t.Errorf("a trailer-less commit keeps its sha and gains no task: %+v", unattributed)
	}

	unknown := &client.LagCollision{LandedInSHA: "abc1234"}
	resolveLanded(snap, unknown, []string{"CY-999"})
	if unknown.LandedByTaskID != 0 {
		t.Errorf("a ref naming no task must not be invented: %+v", unknown)
	}
}
