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
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/output"
	"code.vikunja.io/veans/internal/status"
)

func newClaimCmd() *cobra.Command {
	var (
		force     bool
		branch    string
		fieldsRaw string
		taskIDArg int64
	)
	cmd := &cobra.Command{
		Use:   "claim <PROJ-NN|#NN|NN> | claim --id <task id>",
		Short: "Claim a task: assign the bot, move to In Progress, tag with branch",
		Long: `Atomically takes a task. The server refuses with CONFLICT if another user
already holds it, or if it has left the Todo bucket since you listed it — so
two agents racing for the same task get exactly one winner. Re-claiming a task
you already hold is a no-op.

Pass --force to claim a task that is not in Todo (for example one a human
parked In Review that you have been asked to pick back up). It never
overrides another user's claim.

The veans:branch: label records which branch holds the work. Claiming from the
repository's default branch attaches nothing and says so: claiming before
branching is the natural order, and a label reading veans:branch:main tells
the next reader nothing while making "veans list --branch" under-report. Claim
again once you are on the branch, or pass --branch to name it up front.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fields, err := parseFields(fieldsRaw)
			if err != nil {
				return err
			}
			rt, err := loadRuntime(cmd.Context())
			if err != nil {
				return err
			}
			id, err := rt.taskID(cmd.Context(), args, taskIDArg)
			if err != nil {
				return err
			}
			// What the caller typed, for the refusal message; with --id there
			// is no positional reference to quote.
			ref := fmt.Sprintf("--id %d", taskIDArg)
			if len(args) > 0 {
				ref = args[0]
			}
			task, err := runClaim(cmd.Context(), rt, id, &claimOptions{
				ref:    ref,
				force:  force,
				branch: branch,
				warn:   cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			return rt.printTask(cmd.OutOrStdout(), fields, task)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "claim even if the task is not in the Todo bucket (never overrides another user's claim)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to record in the veans:branch: label (default: the current branch, unless it is the repository's default)")
	cmd.Args = addTaskIDFlag(cmd, &taskIDArg)
	addFieldsFlag(cmd, &fieldsRaw)
	return cmd
}

type claimOptions struct {
	// ref is the task as the caller typed it, for error messages.
	ref    string
	force  bool
	branch string
	warn   io.Writer
}

// runClaim takes the task and records the branch. It is safe to repeat:
// claiming a task the caller already holds is answered by the server as a
// no-op inside the same transaction that would have made the claim, and the
// branch label is only attached when the task does not carry it yet. Nothing
// here decides "is it already mine" on the client — that check-then-act is
// the race the atomic endpoint exists to close.
func runClaim(ctx context.Context, rt *runtime, id int64, o *claimOptions) (*client.Task, error) {
	inProgress, err := status.BucketID(status.InProgress, rt.cfg.Buckets)
	if err != nil {
		return nil, err
	}
	expected := rt.cfg.Buckets.Todo
	if o.force {
		expected = 0
	}

	// One request does the check, the bucket move and the assignment
	// in a single server-side transaction; a 409 here means we lost
	// the race and must not touch the task further.
	claimed, err := rt.client.ClaimTask(ctx, id, rt.cfg.ViewID, inProgress, expected)
	if err != nil {
		var oe *output.Error
		if errors.As(err, &oe) && oe.Code == output.CodeConflict {
			return nil, output.Wrap(output.CodeConflict, err,
				"cannot claim task %s: %v — it is held by someone else, left Todo, is blocked, or owns a path another task has leased; run `veans ready` and pick another (use --force to claim from another bucket)",
				o.ref, err)
		}
		return nil, err
	}

	// From here on the task is ours, so a failure reports what already
	// happened instead of looking like a claim that never took.
	progress := &steps{task: rt.cfg.FormatTaskID(claimed.Index), completed: []string{"claim"}}

	task, err := rt.client.GetTask(ctx, id)
	if err != nil {
		return nil, progress.fail("read task", err)
	}

	// Tag with the branch label, if there is a branch worth recording.
	// A label is bookkeeping, not part of the claim, so it stays
	// outside the transaction. Checked against the task first: a claim
	// runs on every start of work, and re-attaching a label the task
	// already carries is rejected by the server.
	//
	// Re-claiming is how the label arrives late: a worker who claims
	// first and branches second claims again afterwards, and this
	// attaches the real branch then.
	branch := o.branch
	if branch == "" {
		branch = claimBranch(ctx, o.warn)
	}
	if branch != "" {
		labelTitle := branchLabel(branch)
		if !taskHasLabel(task, labelTitle) {
			step := "label " + labelTitle
			l, err := getOrCreateLabelByTitle(ctx, rt.client, labelTitle)
			if err != nil {
				return nil, progress.fail(step, err)
			}
			if err := rt.client.AddLabelToTask(ctx, id, l.ID); err != nil {
				return nil, progress.fail(step, err)
			}
			progress.done(step)
			if task, err = rt.client.GetTask(ctx, id); err != nil {
				return nil, progress.fail("read task", err)
			}
		}
	}

	return task, nil
}

// claimBranch decides what the veans:branch: label should say, and returns ""
// when the honest answer is nothing.
//
// The default branch is not a claim. Claiming before branching is the natural
// order — you often branch off the ticket — so recording whatever HEAD happens
// to be produces a board where a third of the tasks say veans:branch:main,
// which tells the next reader nothing and makes `veans list --branch` silently
// under-report. Warn and attach nothing rather than record a falsehood.
func claimBranch(ctx context.Context, warn io.Writer) string {
	branch := currentGitBranch(ctx)
	if branch == "" {
		return ""
	}
	if def := defaultBranch(ctx); def != "" && branch == def {
		fmt.Fprintf(warn, "warning: claiming from %s, the repository's default branch — no veans:branch: label was attached.\n", branch)
		fmt.Fprintf(warn, "         Claim again once you are on the working branch, or pass --branch to name it now.\n")
		return ""
	}
	return branch
}
