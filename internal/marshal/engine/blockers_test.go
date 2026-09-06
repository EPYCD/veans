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
	"strings"
	"testing"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/marshal/board"
)

func task(id int64, identifier, title string, owned ...string) *client.Task {
	t := &client.Task{ID: id, Identifier: identifier, Title: title}
	if len(owned) > 0 {
		t.Scope = &client.TaskScope{PathsOwned: owned}
	}
	return t
}

// TestLeaseBlockers is the queue-aware half of enforcement point 1. The lease
// system manufactures stale bases: #43 holds a file, #51 is correctly refused
// the claim, #43 merges, #51 claims and cuts a worktree — from a base that
// predates #43's merge, in exactly the file #43 changed. The lease did its job
// perfectly and produced a guaranteed conflict as a side effect.
func TestLeaseBlockers(t *testing.T) {
	held := task(43, "#43", "The write door", "app/src/server/db/schema.ts")
	mine := task(51, "#51", "The depot-pinned login", "app/src/server/db/schema.ts", "app/src/lib/x.ts")

	t.Run("a live lease on a claimed path blocks", func(t *testing.T) {
		snap := &board.Snapshot{
			Tasks:  []*client.Task{held, mine},
			Leases: []*client.TaskPathLease{{TaskID: 43, Pattern: "app/src/server/db/schema.ts"}},
		}
		got := leaseBlockers(snap, mine)
		if len(got) != 1 {
			t.Fatalf("got %d blockers, want 1: %+v", len(got), got)
		}
		if got[0].TaskID != 43 || got[0].Pattern != "app/src/server/db/schema.ts" || !got[0].Leased {
			t.Errorf("blocker = %+v", got[0])
		}
	})

	t.Run("a declared but unleased claim does not block", func(t *testing.T) {
		// Nobody is editing those files yet, so nothing is about to move
		// under this worker. Refusing here would stall work for no reason.
		snap := &board.Snapshot{Tasks: []*client.Task{held, mine}}
		if got := leaseBlockers(snap, mine); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("a task's own lease is not its own blocker", func(t *testing.T) {
		snap := &board.Snapshot{
			Tasks:  []*client.Task{mine},
			Leases: []*client.TaskPathLease{{TaskID: 51, Pattern: "app/src/server/db/schema.ts"}},
		}
		if got := leaseBlockers(snap, mine); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("a lease outliving its task is a health finding, not a blocker", func(t *testing.T) {
		snap := &board.Snapshot{
			Tasks:  []*client.Task{mine},
			Leases: []*client.TaskPathLease{{TaskID: 99, Pattern: "app/src/server/db/schema.ts"}},
		}
		if got := leaseBlockers(snap, mine); len(got) != 0 {
			t.Fatalf("a dangling lease must not hold up a worker: %+v", got)
		}
	})

	t.Run("overlap counts, not string equality", func(t *testing.T) {
		holder := task(43, "#43", "The engine", "app/packages/engine/**")
		claimer := task(51, "#51", "Referral", "app/packages/engine/src/referral.ts")
		snap := &board.Snapshot{
			Tasks:  []*client.Task{holder, claimer},
			Leases: []*client.TaskPathLease{{TaskID: 43, Pattern: "app/packages/engine/**"}},
		}
		if got := leaseBlockers(snap, claimer); len(got) != 1 {
			t.Fatalf("a subtree lease must block a file inside it: %+v", got)
		}
	})

	t.Run("a task with no claim has nothing to be blocked on", func(t *testing.T) {
		snap := &board.Snapshot{
			Tasks:  []*client.Task{held, task(52, "#52", "no scope")},
			Leases: []*client.TaskPathLease{{TaskID: 43, Pattern: "app/src/server/db/schema.ts"}},
		}
		if got := leaseBlockers(snap, task(52, "#52", "no scope")); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})
}

// TestRefusalMessageMany keeps a long list readable. Past a handful the paths
// stop being evidence and start being wallpaper, and "stale in that file" is
// wrong once there are six of them.
func TestRefusalMessageMany(t *testing.T) {
	blockers := []PathHolder{}
	for _, p := range []string{"a.ts", "b.ts", "c.ts", "d.ts", "e.ts", "f.ts"} {
		blockers = append(blockers, PathHolder{TaskRef: TaskRef{TaskID: 48, Identifier: "#48"}, Pattern: p, Leased: true})
	}
	msg := refusalMessage(task(44, "#44", "E7.3"), blockers)
	for _, want := range []string{"#44 claims 6 paths held by open tasks", "a.ts (#48)", "and 2 more", "those files", "Wait for #48"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "e.ts") {
		t.Errorf("the list must be capped, not exhaustive:\n%s", msg)
	}
	if strings.Contains(msg, "that file the moment") {
		t.Errorf("singular phrasing for a list of six:\n%s", msg)
	}
}

// TestRefusalMessageTwoHolders names everyone who has to finish, not just the
// first: waiting for one of two is still being blocked.
func TestRefusalMessageTwoHolders(t *testing.T) {
	msg := refusalMessage(task(44, "#44", "E7.3"), []PathHolder{
		{TaskRef: TaskRef{TaskID: 48, Identifier: "#48"}, Pattern: "a.ts", Leased: true},
		{TaskRef: TaskRef{TaskID: 51, Identifier: "#51"}, Pattern: "b.ts", Leased: true},
	})
	if !strings.Contains(msg, "Wait for #48 and #51") {
		t.Errorf("both holders must be named:\n%s", msg)
	}
}

// TestRefusalMessage: a refusal that does not say who to wait for is a refusal
// that gets forced.
func TestRefusalMessage(t *testing.T) {
	msg := refusalMessage(
		task(51, "#51", "The depot-pinned login", "app/src/server/db/schema.ts"),
		[]PathHolder{{TaskRef: TaskRef{TaskID: 46, Identifier: "#46"}, Pattern: "app/src/server/db/schema.ts", Leased: true}},
	)
	for _, want := range []string{
		"#51 claims app/src/server/db/schema.ts",
		"held by #46 (in progress)",
		"stale in that file the moment #46 merges",
		"Wait for #46, or re-scope",
		"--force",
		"ledger",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
}
