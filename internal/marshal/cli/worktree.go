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

package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"code.vikunja.io/veans/internal/marshal/ledger"
	"code.vikunja.io/veans/internal/output"
)

func newWorktreeCmd() *cobra.Command {
	var worker string
	var force bool
	cmd := &cobra.Command{
		Use:   "worktree <task>",
		Short: "The exact worktree command for a story, cut from the integration branch",
		Long: `Derives the branch and directory from the story id, allocates an unused
database URI and port to the worker, records the checkout so a second worker
resolving to the same path is refused, and prints the commands to run.

The worktree is fetched and cut from the integration branch explicitly, so a
worker starts current. Without a start point "git worktree add" branches from
whatever the invoking checkout's HEAD happens to be — for a clone nobody has
pulled in a week, a week-old base in the files you are about to edit.

It is refused when another open task holds a lease on a path this one claims:
that worker's base is guaranteed to move under exactly those files the moment
the holder merges, so cutting now manufactures the conflict the lease exists
to prevent. Wait for the holder, re-scope, or pass --force — which is recorded
in the ledger.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			t, err := resolveTask(cmd.Context(), e, args[0])
			if err != nil {
				return err
			}
			if worker == "" {
				worker = e.Board.Identity
			}
			snap, err := e.Board.Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			plan, err := e.PlanWorktree(cmd.Context(), snap, t, worker, force)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), plan)
		},
	}
	cmd.Flags().StringVar(&worker, "worker", "", "who the checkout belongs to (default: the token's identity)")
	cmd.Flags().BoolVar(&force, "force", false, "cut the worktree even though an open task holds a lease on a path this one claims (recorded in the ledger)")
	return cmd
}

func newPoolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "The per-worker database and port allocations",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "Allocations, local worktrees, board agents and checkout conflicts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			w, err := e.Workers(cmd.Context())
			if err != nil {
				return err
			}
			if err := emit(cmd.OutOrStdout(), w); err != nil {
				return err
			}
			if len(w.Conflicts) > 0 {
				return output.New(output.CodeConflict, "%d checkout conflict(s)", len(w.Conflicts))
			}
			return nil
		},
	}
	release := &cobra.Command{
		Use:   "release <task-id|worker>",
		Short: "Free a task's or a worker's database and port (done automatically on merge)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			var freed any
			if id, perr := strconv.ParseInt(args[0], 10, 64); perr == nil {
				freed, err = e.Pool.ReleaseTask(id)
			} else {
				freed, err = e.Pool.ReleaseWorker(args[0])
			}
			if err != nil {
				return err
			}
			e.Log(ledger.Entry{Action: "free", Subject: args[0], Outcome: "ok"})
			return emit(cmd.OutOrStdout(), map[string]any{"released": freed})
		},
	}
	cmd.AddCommand(list, release)
	return cmd
}
