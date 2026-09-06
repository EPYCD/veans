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

import "context"

// RouteGroup mirrors models.APITokenRoute on the wire — the per-action
// detail object is opaque to us.
type RouteGroup map[string]struct {
	Path   string `json:"path"`
	Method string `json:"method"`
}

// Routes returns the API token route map. Used during bootstrap to
// negotiate exactly which permission groups+actions exist on this Vikunja
// instance, so the bot's API token only requests scopes the server knows
// about — avoiding hard-coding a permission list that could drift.
func (c *Client) Routes(ctx context.Context) (map[string]RouteGroup, error) {
	out := map[string]RouteGroup{}
	if err := c.Do(ctx, "GET", "/routes", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PermissionsForBot picks a curated subset of route groups the veans bot
// needs and projects the available actions of each. Groups not present on
// the server are silently dropped, so the resulting permission map is
// always valid for POST /tokens regardless of Vikunja version.
//
// The action names reflect Vikunja's actual route map (see GET /routes):
// bucket CRUD and the bucket-task move endpoint live under the `projects`
// group as `views_buckets*` and `views_buckets_tasks`, not a separate
// `buckets` group.
func PermissionsForBot(routes map[string]RouteGroup) map[string][]string {
	wanted := map[string][]string{
		// Read + write tasks across the project. The bot creates, updates,
		// and reads tasks; it doesn't delete (humans/merge hook close).
		// `claim` is the atomic POST /tasks/{id}/claim; older servers without
		// it are dropped by the intersection below and `veans claim` fails
		// with a clear error rather than falling back to a racy sequence.
		// scope*/leases* are the task-scope and path-lease endpoints; like
		// `claim` they are simply absent on older servers.
		"tasks": {
			"read_one", "read_all", "create", "update", "position",
			"read", "update_bulk", "claim",
			"scope", "scope_get", "scope_put", "scope_delete",
			"leases", "leases_delete", "leases_heartbeat", "receipts",
			// Branch lag: Marshal writes it, veans check/sync/update read it.
			// All four spellings for the same reason as scope — which of the
			// three methods takes the bare subkey depends on unspecified
			// route-init order, and the runtime intersection drops the rest.
			"lag", "lag_get", "lag_put", "lag_delete",
		},
		// Project access: read project metadata, manage buckets & move
		// tasks between them. tasks_by-index resolves #NN / PROJ-NN.
		// The bucket-task MOVE (PUT .../buckets/:bucket/tasks) and the
		// buckets-with-tasks LIST (GET .../buckets/tasks) collide on subkey
		// `views_buckets_tasks`; which one gets the bare key vs the
		// `_put`-suffixed key depends on unspecified route-init order, so we
		// request BOTH and let the runtime intersection drop whichever the
		// server didn't register.
		"projects": {
			"read_one", "read_all", "tasks_by-index", "tasks_by_index",
			"views_buckets", "views_buckets_put", "views_buckets_post",
			"views_buckets_delete", "views_buckets_tasks", "views_buckets_tasks_put",
			"leases", "views_readiness", "scope_check", "plan", "plan_get", "agents",
		},
		"projects_views":  {"read_one", "read_all"},
		"labels":          {"read_one", "read_all", "create", "update", "delete"},
		"tasks_comments":  {"read_one", "read_all", "create", "update", "delete"},
		"tasks_relations": {"create", "delete"},
		"tasks_assignees": {"read_all", "create", "delete", "update_bulk"},
		"tasks_labels":    {"create", "delete", "read_all", "update_bulk"},
	}
	return pickPermissions(routes, wanted)
}

// PermissionsForCI is the receipt bot's grant: post receipts, close and
// comment on tasks, park them in review, run the PR scope check, and nothing
// a worker needs.
//
// scope_check is not optional. It is the endpoint the merge hook's `check`
// mode exists to call, and this is the token that mode runs with — the CI
// token `marshal setup` prints for the WORKMAN_TOKEN repository secret, not
// the worker token `veans login` mints. Without it the hook got a 401 on
// POST /projects/{id}/scope-check while succeeding on everything else in the
// same job, which reads as an expired token rather than a missing grant. The
// hook steps are deliberately continue-on-error, so the whole PR check failed
// silently and green.
func PermissionsForCI(routes map[string]RouteGroup) map[string][]string {
	wanted := map[string][]string{
		// lag is read-only for CI: the merge hook's check mode refuses a PR
		// whose branch is behind in a file it owns, and needs to see the
		// record to do it. It has no business writing one — that is Marshal's
		// job, and Marshal has its own identity.
		"tasks":          {"read_one", "update", "read", "receipts", "receipts_post", "lag", "lag_get"},
		"tasks_comments": {"create", "read_all"},
		"projects":       {"read_one", "tasks_by-index", "tasks_by_index", "views_buckets_tasks", "views_buckets_tasks_put", "leases", "scope_check", "webhooks", "webhooks_get"},
		"projects_views": {"read_one", "read_all"},
	}
	return pickPermissions(routes, wanted)
}

func pickPermissions(routes map[string]RouteGroup, wanted map[string][]string) map[string][]string {
	out := map[string][]string{}
	for group, actions := range wanted {
		avail, ok := routes[group]
		if !ok {
			continue
		}
		var picked []string
		for _, a := range actions {
			if _, has := avail[a]; has {
				picked = append(picked, a)
			}
		}
		if len(picked) > 0 {
			out[group] = picked
		}
	}
	return out
}
