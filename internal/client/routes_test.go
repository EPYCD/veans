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

package client

import (
	"slices"
	"testing"
)

// serverRoutes is what GET /routes reports for the groups these grants draw
// on. Only the keys matter; the detail object is opaque.
func serverRoutes() map[string]RouteGroup {
	group := func(actions ...string) RouteGroup {
		g := RouteGroup{}
		for _, a := range actions {
			g[a] = struct {
				Path   string `json:"path"`
				Method string `json:"method"`
			}{}
		}
		return g
	}
	return map[string]RouteGroup{
		"tasks": group("read_one", "read_all", "create", "update", "position", "read",
			"update_bulk", "claim", "scope", "scope_get", "scope_put", "scope_delete",
			"leases", "leases_delete", "leases_heartbeat", "receipts", "receipts_post",
			"lag", "lag_put", "lag_delete"),
		"projects": group("read_one", "read_all", "tasks_by-index", "views_buckets",
			"views_buckets_put", "views_buckets_post", "views_buckets_delete",
			"views_buckets_tasks", "leases", "views_readiness", "scope_check", "plan",
			"agents", "webhooks", "webhooks_get"),
		"projects_views":  group("read_one", "read_all"),
		"labels":          group("read_one", "read_all", "create", "update", "delete"),
		"tasks_comments":  group("read_one", "read_all", "create", "update", "delete"),
		"tasks_relations": group("create", "delete"),
		"tasks_assignees": group("read_all", "create", "delete", "update_bulk"),
		"tasks_labels":    group("create", "delete", "read_all", "update_bulk"),
	}
}

// TestPermissionsForCIGrantsScopeCheck is the regression test for a PR check
// that failed silently for every project scaffolded from this action.
//
// `mode: check` calls POST /projects/{id}/scope-check. It runs with the CI
// token `marshal setup` prints, whose grant is PermissionsForCI — and that
// list did not contain scope_check. In one CI job on the same secret,
// `mode: opened` succeeded (tasks, comments and buckets are all granted)
// while `mode: check` got HTTP 401, which reads as an expired token rather
// than a missing grant. The hook steps are continue-on-error, so the check
// failed green.
func TestPermissionsForCIGrantsScopeCheck(t *testing.T) {
	perms := PermissionsForCI(serverRoutes())
	if !slices.Contains(perms["projects"], "scope_check") {
		t.Fatalf("the CI token must be able to run the project's own scope check; projects grant = %v", perms["projects"])
	}
}

// TestPermissionsForBotGrantsScopeCheck pins the worker side too, since a
// local `veans check --staged` hits the same endpoint.
func TestPermissionsForBotGrantsScopeCheck(t *testing.T) {
	perms := PermissionsForBot(serverRoutes())
	if !slices.Contains(perms["projects"], "scope_check") {
		t.Fatalf("projects grant = %v", perms["projects"])
	}
}

// TestGrantsCoverBranchLag: the merge hook's check mode refuses a PR whose
// branch is behind in a file it owns, which means reading the lag record. A
// route added to the server without being added here is silently dropped by
// pickPermissions and fails as a 401 that reads like an expired token — the
// exact shape of the scope-check outage.
func TestGrantsCoverBranchLag(t *testing.T) {
	ci := PermissionsForCI(serverRoutes())
	if !slices.Contains(ci["tasks"], "lag") {
		t.Errorf("the CI token must be able to read lag: %v", ci["tasks"])
	}
	// It has no business writing one; that is Marshal's job.
	if slices.Contains(ci["tasks"], "lag_put") || slices.Contains(ci["tasks"], "lag_delete") {
		t.Errorf("the CI token must not write lag: %v", ci["tasks"])
	}

	bot := PermissionsForBot(serverRoutes())
	for _, want := range []string{"lag", "lag_put", "lag_delete"} {
		if !slices.Contains(bot["tasks"], want) {
			t.Errorf("the worker token needs %q: %v", want, bot["tasks"])
		}
	}
}

// TestPermissionsForCIStaysNarrow: adding scope_check must not turn the CI
// identity into a worker. It posts receipts and moves tasks; it does not take
// work, write scopes or hold leases.
func TestPermissionsForCIStaysNarrow(t *testing.T) {
	perms := PermissionsForCI(serverRoutes())
	for _, forbidden := range []string{"claim", "scope_put", "scope_delete", "leases_delete", "create"} {
		if slices.Contains(perms["tasks"], forbidden) {
			t.Errorf("the CI token must not be able to %q: %v", forbidden, perms["tasks"])
		}
	}
	if _, has := perms["tasks_assignees"]; has {
		t.Errorf("the CI token has no business assigning anyone: %v", perms["tasks_assignees"])
	}
}

// TestPickPermissionsDropsWhatTheServerLacks is the mechanism that made the
// omission invisible: a requested action the server does not register is
// silently dropped rather than failing the mint, so a typo or a rename looks
// exactly like an older server.
func TestPickPermissionsDropsWhatTheServerLacks(t *testing.T) {
	routes := map[string]RouteGroup{"projects": serverRoutes()["projects"]}
	got := pickPermissions(routes, map[string][]string{
		"projects": {"read_one", "scope-check", "nonexistent"},
		"labels":   {"create"},
	})
	if !slices.Equal(got["projects"], []string{"read_one"}) {
		t.Errorf("projects = %v, want only read_one", got["projects"])
	}
	if _, has := got["labels"]; has {
		t.Errorf("a group the server does not have must be dropped entirely: %v", got)
	}
}
