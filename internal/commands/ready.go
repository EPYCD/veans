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
	"strings"

	"github.com/spf13/cobra"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/status"
)

func newReadyCmd() *cobra.Command {
	var bucket string
	cmd := &cobra.Command{
		Use:   "ready",
		Short: "The ready queue: every Todo task with whether it can be claimed and, if not, why",
		Long: `Asks the server for the readiness of every open task in a bucket, Todo by
default. Each row carries the task, "ready", and the reasons it is not:
"assigned" (someone holds it), "blocked" (an unfinished blocker, listed under
blocked_by), "lease_conflict" (one of its paths-owned overlaps a lease held by
an in-progress task, listed under lease_conflicts) or "lag" (the integration
branch has moved inside a path it owns, detailed under lag).

Pass --bucket in-progress to see the work already claimed, which is where lag
lives: a task in Todo has no branch yet, so it cannot be behind one. Every row
carries its lag object when it has one, at any severity — only "owned" makes a
task not ready, while "affected" and "elsewhere" are reported and gate nothing.

` + "`veans list --ready`" + ` is the same query reduced to the tasks that are ready.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rt, err := loadRuntime()
			if err != nil {
				return err
			}
			bucketID, err := readyBucketID(rt, bucket)
			if err != nil {
				return err
			}
			rows, err := readinessFor(cmd.Context(), rt, bucketID)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
		},
	}
	cmd.Flags().StringVar(&bucket, "bucket", "", "which bucket to report on: todo (default), in-progress, in-review, done, scrapped")
	return cmd
}

// readyBucketID turns the --bucket flag into an id, defaulting to Todo.
func readyBucketID(rt *runtime, name string) (int64, error) {
	if strings.TrimSpace(name) == "" {
		return rt.cfg.Buckets.Todo, nil
	}
	st, err := status.Parse(name)
	if err != nil {
		return 0, err
	}
	return status.BucketID(st, rt.cfg.Buckets)
}

// readyQueue fetches the Todo bucket's readiness, which is what every existing
// caller means by "the queue".
func readyQueue(ctx context.Context, rt *runtime) ([]*client.TaskReadiness, error) {
	return readinessFor(ctx, rt, rt.cfg.Buckets.Todo)
}

// readinessFor fetches one bucket's readiness. The server does not expand
// buckets on these tasks, so the bucket is stamped on here — every row is in
// the requested bucket by construction — which keeps CurrentBucketID and the
// status filters working on them.
func readinessFor(ctx context.Context, rt *runtime, bucketID int64) ([]*client.TaskReadiness, error) {
	rows, err := rt.client.ProjectReadiness(ctx, rt.cfg.ProjectID, rt.cfg.ViewID, bucketID)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []*client.TaskReadiness{}
	}
	for _, r := range rows {
		if r.Task != nil {
			r.Task.BucketID = bucketID
		}
	}
	return rows, nil
}
