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

package commands

import (
	"strings"
	"testing"
	"time"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/output"
)

func lagWith(severity string, collisions ...*client.LagCollision) *client.TaskLag {
	return &client.TaskLag{
		Branch: "e9.4-depot", Base: "origin/main", CommitsBehind: 7,
		Severity: severity, Collisions: collisions, ComputedAt: time.Now(),
	}
}

// TestLagRefusal is the review gate. A branch behind the integration branch in
// a file it OWNS will conflict, so the diff a reviewer reads is not the diff
// that will merge and the gates that just passed did not run against what will
// land.
func TestLagRefusal(t *testing.T) {
	task := &client.Task{ID: 51, Identifier: "#51"}
	lag := lagWith(client.LagSeverityOwned, &client.LagCollision{
		Path: "captain-yard-web/src/server/db/schema.ts", Severity: client.LagSeverityOwned,
		LandedByIdentifier: "#43", LandedInSHA: "141e6cd8f0a",
	})

	err := lagRefusal(task, lag)
	if err == nil {
		t.Fatal("an owned collision must refuse review")
	}
	var oe *output.Error
	if !asOutputError(err, &oe) || oe.Code != output.CodeConflict {
		t.Fatalf("the refusal must be a CONFLICT the tooling already handles: %#v", err)
	}
	for _, want := range []string{
		"#51 is behind origin/main in a file it owns",
		"captain-yard-web/src/server/db/schema.ts — landed by #43 (141e6cd)",
		"Rebase and re-run the gates: veans sync #51",
		"Override with --force",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%s", want, err.Error())
		}
	}
}

// TestLagRefusalGatesOnlyOnOwned is the rule the whole ladder rests on.
// affected collisions are common and usually harmless; failing on them would
// train everyone to pass --force by reflex, which would then also defeat the
// gate on owned.
func TestLagRefusalGatesOnlyOnOwned(t *testing.T) {
	task := &client.Task{ID: 51, Identifier: "#51"}
	for _, severity := range []string{client.LagSeverityAffected, client.LagSeverityElsewhere, ""} {
		lag := lagWith(severity, &client.LagCollision{Path: "x.ts", Severity: severity})
		if err := lagRefusal(task, lag); err != nil {
			t.Errorf("severity %q must gate nothing, got %v", severity, err)
		}
	}
	if err := lagRefusal(task, nil); err != nil {
		t.Errorf("no lag record gates nothing, got %v", err)
	}
}

// TestLagRefusalNamesAReferenceTheCLIAccepts: a refusal that tells you to run
// a command the CLI would reject is a refusal nobody can act on.
func TestLagRefusalNamesAReferenceTheCLIAccepts(t *testing.T) {
	lag := lagWith(client.LagSeverityOwned, &client.LagCollision{Path: "x.ts", Severity: client.LagSeverityOwned})

	withIdentifier := lagRefusal(&client.Task{ID: 51, Identifier: "CY-51"}, lag)
	if !strings.Contains(withIdentifier.Error(), "veans sync CY-51") {
		t.Errorf("want the identifier: %s", withIdentifier.Error())
	}
	// A task with no identifier falls back to the numeric id, which veans also
	// accepts — not "#0" or an empty argument.
	withoutIdentifier := lagRefusal(&client.Task{ID: 51}, lag)
	if !strings.Contains(withoutIdentifier.Error(), "veans sync 51") {
		t.Errorf("want the id: %s", withoutIdentifier.Error())
	}
}

// TestLandedBy: a commit with no Refs: trailer keeps its sha and says why it
// names no task. Dropping it would hide a real collision for the sake of
// tidiness.
func TestLandedBy(t *testing.T) {
	cases := []struct {
		c    *client.LagCollision
		want string
	}{
		{&client.LagCollision{LandedByIdentifier: "#43", LandedInSHA: "141e6cd8f0a"}, "#43 (141e6cd)"},
		{&client.LagCollision{LandedInSHA: "141e6cd8f0a"}, "141e6cd (no Refs: trailer)"},
		{&client.LagCollision{LandedByIdentifier: "#43"}, "#43"},
		{&client.LagCollision{}, "an unknown commit"},
	}
	for _, tc := range cases {
		if got := landedBy(tc.c); got != tc.want {
			t.Errorf("landedBy(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

// TestWriteSyncPlanIsAdvisory: sync prints commands and runs none of them. An
// agent's worktree is frequently dirty mid-edit, and a half-finished rebase in
// one is materially worse than a stale branch.
func TestWriteSyncPlanIsAdvisory(t *testing.T) {
	var out strings.Builder
	writeSyncPlan(&out, &client.Task{ID: 51, Identifier: "#51"}, lagWith(
		client.LagSeverityOwned,
		&client.LagCollision{Path: "app/src/db/schema.ts", Severity: client.LagSeverityOwned, LandedByIdentifier: "#43", LandedInSHA: "141e6cd"},
		&client.LagCollision{Path: "app/packages/engine/x.ts", Severity: client.LagSeverityAffected, LandedByIdentifier: "#43"},
	))
	got := out.String()
	for _, want := range []string{
		"#51 is 7 commits behind origin/main",
		"OWNED — you will conflict",
		"AFFECTED — your assumptions may be stale",
		"git fetch origin",
		"git rebase origin/main",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestWriteSyncPlanWhenCurrent(t *testing.T) {
	var out strings.Builder
	writeSyncPlan(&out, &client.Task{ID: 51, Identifier: "#51"}, &client.TaskLag{Base: "origin/main"})
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("got %q", out.String())
	}
}

func asOutputError(err error, target **output.Error) bool {
	oe, ok := err.(*output.Error)
	if ok {
		*target = oe
	}
	return ok
}
