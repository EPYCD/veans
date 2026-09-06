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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// unclaimServer serves one task with the given assignees, labels and bucket,
// and records every call. It is deliberately strict: an unexpected request
// fails the test, so a change that starts touching something new has to say so.
func unclaimServer(t *testing.T, task map[string]any) (*httptest.Server, *[]recordedCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []recordedCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, recordedCall{method: r.Method, path: r.URL.Path})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tasks/42":
			_ = json.NewEncoder(w).Encode(task)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2/tasks/42/leases":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2/tasks/42/assignees/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v2/tasks/42/labels/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/tasks"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func called(calls []recordedCall, method, path string) bool {
	for _, c := range calls {
		if c.method == method && c.path == path {
			return true
		}
	}
	return false
}

// TestRunUnclaim_HandsTheTaskBack is the regression test for the gap that left
// two CapYard tickets permanently unclaimable. An agent could move the bucket
// back and drop its leases and still not have handed the task back, because
// the assignee is what makes a task not ready and nothing in the CLI could
// remove one. `veans ready` said reasons=["assigned"], blocked_by=[],
// lease_conflicts=[] — dead with no blocker and no conflict.
func TestRunUnclaim_HandsTheTaskBack(t *testing.T) {
	srv, calls := unclaimServer(t, map[string]any{
		"id": 42, "title": "t", "updated": "2026-01-01T00:00:00Z",
		"assignees": []map[string]any{{"id": 3, "username": "bot-akshat"}},
		"labels": []map[string]any{
			{"id": 9, "title": "veans:branch:e13.1-importer"},
			{"id": 8, "title": "epic"},
		},
		"buckets": []map[string]any{{"project_view_id": 1, "id": 11}},
	})
	rt := newTestRuntime(srv.URL)
	rt.cfg.Bot.UserID = 3

	res, err := runUnclaim(context.Background(), rt, 42, false)
	if err != nil {
		t.Fatalf("runUnclaim: %v", err)
	}

	if !reflect.DeepEqual(res.Unassigned, []string{"bot-akshat"}) {
		t.Errorf("Unassigned = %v", res.Unassigned)
	}
	if !res.ReleasedLeases || !res.MovedToTodo {
		t.Errorf("result = %+v", res)
	}
	if !reflect.DeepEqual(res.RemovedLabels, []string{"veans:branch:e13.1-importer"}) {
		t.Errorf("RemovedLabels = %v", res.RemovedLabels)
	}

	// All four things must actually happen; any one of them left out is a
	// ticket that stays stuck.
	for _, want := range []struct{ method, path string }{
		{http.MethodDelete, "/api/v2/tasks/42/leases"},
		{http.MethodDelete, "/api/v2/tasks/42/assignees/3"},
		{http.MethodPut, "/api/v2/projects/7/views/1/buckets/10/tasks"},
		{http.MethodDelete, "/api/v2/tasks/42/labels/9"},
	} {
		if !called(*calls, want.method, want.path) {
			t.Errorf("missing call: %s %s\ngot %+v", want.method, want.path, *calls)
		}
	}
	// A label a human put on the card is not ours to remove.
	if called(*calls, http.MethodDelete, "/api/v2/tasks/42/labels/8") {
		t.Error("only the veans:branch: label may be removed")
	}
}

// TestRunUnclaim_Idempotent: a second run must change nothing and succeed. An
// agent retrying after a network blip should not get an error that sends it
// looking for a problem that is not there.
func TestRunUnclaim_Idempotent(t *testing.T) {
	srv, calls := unclaimServer(t, map[string]any{
		"id": 42, "title": "t", "updated": "2026-01-01T00:00:00Z",
		"assignees": []map[string]any{},
		"labels":    []map[string]any{},
		"buckets":   []map[string]any{{"project_view_id": 1, "id": 10}},
	})
	rt := newTestRuntime(srv.URL)
	rt.cfg.Bot.UserID = 3

	res, err := runUnclaim(context.Background(), rt, 42, false)
	if err != nil {
		t.Fatalf("runUnclaim on an already-unclaimed task must succeed: %v", err)
	}
	if !res.AlreadyUnowned || len(res.Unassigned) != 0 || res.MovedToTodo {
		t.Errorf("second run should be a no-op: %+v", res)
	}
	if called(*calls, http.MethodPut, "/api/v2/projects/7/views/1/buckets/10/tasks") {
		t.Error("a task already in Todo must not be moved again")
	}
}

// TestRunUnclaim_RefusesAnotherUsersTask: unclaim is for handing back your own
// work, not for taking a colleague's off them.
func TestRunUnclaim_RefusesAnotherUsersTask(t *testing.T) {
	task := map[string]any{
		"id": 42, "title": "t", "updated": "2026-01-01T00:00:00Z",
		"assignees": []map[string]any{{"id": 9, "username": "someone-else"}},
		"buckets":   []map[string]any{{"project_view_id": 1, "id": 11}},
	}
	srv, calls := unclaimServer(t, task)
	rt := newTestRuntime(srv.URL)
	rt.cfg.Bot.UserID = 3

	_, err := runUnclaim(context.Background(), rt, 42, false)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "someone-else") {
		t.Errorf("the refusal must name the holder: %v", err)
	}
	// Refused means nothing happened — not even the leases.
	if called(*calls, http.MethodDelete, "/api/v2/tasks/42/leases") {
		t.Error("a refused unclaim must not release leases")
	}

	// --force is the escape hatch for clearing up after a crashed session.
	srv2, calls2 := unclaimServer(t, task)
	rt2 := newTestRuntime(srv2.URL)
	rt2.cfg.Bot.UserID = 3
	res, err := runUnclaim(context.Background(), rt2, 42, true)
	if err != nil {
		t.Fatalf("--force: %v", err)
	}
	if !reflect.DeepEqual(res.Unassigned, []string{"someone-else"}) {
		t.Errorf("Unassigned = %v", res.Unassigned)
	}
	if !called(*calls2, http.MethodDelete, "/api/v2/tasks/42/assignees/9") {
		t.Error("--force must remove the other user's assignment")
	}
}
