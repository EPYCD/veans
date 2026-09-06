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
	"code.vikunja.io/veans/internal/output"
	"code.vikunja.io/veans/internal/status"
)

// unclaimResult reports what actually changed, so a second run visibly does
// nothing rather than looking identical to the first.
type unclaimResult struct {
	Task           *client.Task `json:"task"`
	Unassigned     []string     `json:"unassigned"`
	MovedToTodo    bool         `json:"moved_to_todo"`
	ReleasedLeases bool         `json:"released_leases"`
	RemovedLabels  []string     `json:"removed_labels"`
	AlreadyUnowned bool         `json:"already_unowned"`
}

func newUnclaimCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "unclaim <id>",
		Short: "Hand a task back: drop the assignee, return it to Todo, release leases, remove the branch label",
		Long: `The inverse of claim. Returns a task to the ready queue in one step:
removes the assignee, moves it back to the Todo bucket, releases every path
lease it holds, and removes the veans:branch: label that pointed at work that
is no longer happening.

Use it when you took a ticket and are not going to do it — claimed in error,
blocked by something you cannot move, or handing off. Doing only some of these
leaves the task unclaimable by anyone: a task with an assignee is never ready,
whatever bucket it sits in and whatever the card says.

Idempotent: running it on an unclaimed task in Todo changes nothing and
succeeds. It refuses to take a task away from another user unless you pass
--force, which is for a human clearing up after a crashed session, not for an
agent taking work off a colleague.

This is not "release". That drops leases and deliberately keeps the assignee,
for abandoning the work while keeping the ticket.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := loadRuntime()
			if err != nil {
				return err
			}
			id, err := rt.resolveTaskID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			res, err := runUnclaim(cmd.Context(), rt, id, force)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "unassign a task held by another user (for clearing up after a crashed session)")
	return cmd
}

// runUnclaim performs the whole hand-back. The order is deliberate: leases go
// first because they are what blocks OTHER tasks, then the assignee because
// that is what blocks this one, then the bucket and the label, which are
// presentation. A failure part-way therefore leaves the board more claimable
// than it was, never less.
func runUnclaim(ctx context.Context, rt *runtime, id int64, force bool) (*unclaimResult, error) {
	task, err := rt.client.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}

	holders, others := classifyAssignees(task, rt.cfg.Bot.UserID)
	if len(others) > 0 && !force {
		return nil, output.New(output.CodeConflict,
			"task %d is held by %s, not by you — ask them to hand it back, or pass --force if you are clearing up after a crashed session",
			id, strings.Join(others, ", "))
	}

	res := &unclaimResult{Unassigned: []string{}, RemovedLabels: []string{}}
	res.AlreadyUnowned = len(holders) == 0

	if err := rt.client.ReleaseTaskLeases(ctx, id); err != nil {
		return nil, err
	}
	res.ReleasedLeases = true

	for _, a := range task.Assignees {
		if a == nil {
			continue
		}
		if a.ID != rt.cfg.Bot.UserID && !force {
			continue
		}
		if err := rt.client.RemoveAssignee(ctx, id, a.ID); err != nil {
			return nil, err
		}
		res.Unassigned = append(res.Unassigned, assigneeName(a))
	}

	// Back to Todo. A task nobody is working on that sits in In Progress is a
	// lie the board tells every reader.
	todo, err := status.BucketID(status.Todo, rt.cfg.Buckets)
	if err != nil {
		return nil, err
	}
	if task.CurrentBucketID(rt.cfg.ViewID) != todo {
		if err := rt.client.MoveTaskToBucket(ctx, rt.cfg.ProjectID, rt.cfg.ViewID, todo, id); err != nil {
			return nil, err
		}
		res.MovedToTodo = true
	}

	// The branch label points at work that is no longer happening. Leaving it
	// makes `veans list --branch` report a worker on a branch nobody is on.
	for _, l := range task.Labels {
		if l == nil || !strings.HasPrefix(l.Title, branchLabelPrefix) {
			continue
		}
		if err := rt.client.RemoveLabelFromTask(ctx, id, l.ID); err != nil {
			return nil, err
		}
		res.RemovedLabels = append(res.RemovedLabels, l.Title)
	}

	if res.Task, err = rt.client.GetTask(ctx, id); err != nil {
		return nil, err
	}
	return res, nil
}

// classifyAssignees splits a task's assignees into everyone holding it and
// everyone holding it who is not us.
func classifyAssignees(t *client.Task, botUserID int64) (holders, others []string) {
	for _, a := range t.Assignees {
		if a == nil {
			continue
		}
		name := assigneeName(a)
		holders = append(holders, name)
		if a.ID != botUserID {
			others = append(others, name)
		}
	}
	return holders, others
}

func assigneeName(u *client.User) string {
	if u.Username != "" {
		return u.Username
	}
	return u.Name
}
