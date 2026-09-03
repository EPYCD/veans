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

package board

import (
	"testing"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/marshal/invariants"
)

func parented(id int64, identifier string, parent int64) *client.Task {
	return &client.Task{
		ID:           id,
		Identifier:   identifier,
		Done:         true,
		RelatedTasks: map[string][]*client.Task{"parenttask": {{ID: parent}}},
	}
}

// An epic whose stories have all shipped stays a container. Closed tasks are
// what prove it is one, so they have to reach the checker: without them the
// epic looks like a leaf that forgot to declare paths_owned.
func TestInvariantTasksCompletedEpicStaysAContainer(t *testing.T) {
	epic := &client.Task{ID: 2, Identifier: "#2", Title: "E0 — the contract splits"}
	s := &Snapshot{
		Tasks:     []*client.Task{epic},
		DoneTasks: []*client.Task{parented(17, "#17", 2)},
	}

	tasks, leases := s.InvariantTasks()

	var story *invariants.Task
	for i := range tasks {
		if tasks[i].ID == 17 {
			story = &tasks[i]
		}
	}
	if story == nil {
		t.Fatal("the closed story never reached the checker")
	}
	if !story.Done || story.ParentID != 2 {
		t.Fatalf("closed story lost its parent link: done=%v parent=%d", story.Done, story.ParentID)
	}

	rep := invariants.Check(tasks, leases)
	for _, f := range rep.Findings {
		if f.Code == invariants.CodeNoClaim {
			t.Fatalf("completed epic flagged as a claimless story: %s", f.Message)
		}
	}
	if rep.Containers != 1 {
		t.Fatalf("Containers = %d, want 1", rep.Containers)
	}
	if !rep.OK {
		t.Fatalf("report not OK: %+v", rep.Findings)
	}
}

// The exemption is only for parents. A childless open task with no claim is
// still a finding, so the fix above cannot quietly excuse every task.
func TestInvariantTasksChildlessTaskStillNeedsAClaim(t *testing.T) {
	s := &Snapshot{
		Tasks: []*client.Task{{ID: 9, Identifier: "#9", Title: "a leaf with no claim"}},
	}

	rep := invariants.Check(s.InvariantTasks())

	var found bool
	for _, f := range rep.Findings {
		if f.Code == invariants.CodeNoClaim && len(f.TaskIDs) == 1 && f.TaskIDs[0] == 9 {
			found = true
		}
	}
	if !found {
		t.Fatalf("childless claimless task was not flagged: %+v", rep.Findings)
	}
}
