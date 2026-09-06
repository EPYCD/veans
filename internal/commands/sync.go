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
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/output"
)

func newSyncCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "sync <id>",
		Short: "What you are behind on, and the commands to catch up",
		Long: `Prints why this task's branch is out of date and what to run about it:
the merged tasks whose commits you are behind on, the colliding paths with
their severity, and the exact commands, in your own worktree.

Severity is what makes this worth reading. OWNED means the integration branch
moved inside a file you claimed to edit, so a textual conflict is certain, not
likely. AFFECTED means it moved in something you declared you read but do not
edit: no conflict, but your assumptions and your gates are stale. Anything
outside your scope is counted and not listed, because forty commits behind in
files you never touch is noise.

It changes nothing. It does not fetch, it does not rebase, and it does not
touch the working tree — an agent's worktree is frequently dirty mid-edit, and
a half-finished rebase in one is materially worse than a stale branch. Run the
commands yourself when the tree is clean.

The lag it reads is computed by Marshal on its poll, so a branch you pushed
seconds ago may not be reflected yet.`,
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
			lag, err := rt.client.GetTaskLag(cmd.Context(), id)
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(lag)
			}
			task, err := rt.client.GetTask(cmd.Context(), id)
			if err != nil {
				return err
			}
			writeSyncPlan(cmd.OutOrStdout(), task, lag)
			if lag.Blocking() {
				return output.New(output.CodeConflict, "%s is behind %s in %d file(s) it owns — rebase before opening a PR",
					taskRef(task), lag.Base, len(lag.OwnedCollisions()))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the lag record instead of the plan")
	return cmd
}

// writeSyncPlan renders the advisory. The commands come last and are exact,
// because the point is that a worker can act without composing anything.
func writeSyncPlan(w io.Writer, task *client.Task, lag *client.TaskLag) {
	ref := taskRef(task)
	if lag == nil || lag.CommitsBehind == 0 {
		fmt.Fprintf(w, "%s is up to date with %s.\n", ref, baseOr(lag))
		return
	}

	fmt.Fprintf(w, "%s is %d commit%s behind %s.\n", ref, lag.CommitsBehind, plural(lag.CommitsBehind), lag.Base)

	owned := lag.OwnedCollisions()
	if len(owned) > 0 {
		fmt.Fprintf(w, "\n  OWNED — you will conflict\n")
		writeCollisions(w, owned)
	}
	affected := lag.AffectedCollisions()
	if len(affected) > 0 {
		fmt.Fprintf(w, "\n  AFFECTED — your assumptions may be stale\n")
		writeCollisions(w, affected)
	}
	if len(owned) == 0 && len(affected) == 0 {
		fmt.Fprintf(w, "\n  Nothing you claim has moved. Rebase when convenient.\n")
	}

	dir := "your worktree"
	if branch := lag.Branch; branch != "" {
		dir = "the worktree on " + branch
	}
	fmt.Fprintf(w, "\n  Run, in %s:\n", dir)
	fmt.Fprintf(w, "    git fetch %s\n", remoteOf(lag.Base))
	fmt.Fprintf(w, "    git rebase %s\n", lag.Base)
	fmt.Fprintf(w, "    # then re-run your gates\n")
}

func writeCollisions(w io.Writer, cs []*client.LagCollision) {
	width := 0
	for _, c := range cs {
		if len(c.Path) > width {
			width = len(c.Path)
		}
	}
	for _, c := range cs {
		fmt.Fprintf(w, "    %-*s   ← %s\n", width, c.Path, landedBy(c))
	}
}

// landedBy names who moved the file. A commit with no Refs: trailer keeps its
// sha and says so, rather than being dropped for not being attributable.
func landedBy(c *client.LagCollision) string {
	sha := c.LandedInSHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	switch {
	case c.LandedByIdentifier != "" && sha != "":
		return fmt.Sprintf("%s (%s)", c.LandedByIdentifier, sha)
	case c.LandedByIdentifier != "":
		return c.LandedByIdentifier
	case sha != "":
		return sha + " (no Refs: trailer)"
	default:
		return "an unknown commit"
	}
}

// lagRefusal is the review gate. A task whose branch is behind the integration
// branch in a file it OWNS is not ready for review: the conflict is certain,
// so the diff a reviewer reads is not the diff that will merge, and the gates
// that just passed did not run against what will land.
//
// It returns nil for `affected` and `elsewhere`. Those are reported and gate
// nothing, anywhere — gating on `affected` would make overriding routine, and
// an override typed by reflex defeats the gate on `owned` too.
func lagRefusal(t *client.Task, lag *client.TaskLag) error {
	if !lag.Blocking() {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s is behind %s in a file it owns.", taskRef(t), lag.Base)
	for _, c := range lag.OwnedCollisions() {
		fmt.Fprintf(&b, "\n  %s — landed by %s", c.Path, landedBy(c))
	}
	fmt.Fprintf(&b, "\nRebase and re-run the gates: veans sync %s", taskRefArg(t))
	b.WriteString("\nOverride with --force (recorded on the task).")
	return output.New(output.CodeConflict, "%s", b.String())
}

// taskRefArg is what to type back at veans: the identifier when there is one,
// otherwise the id. A refusal that names a reference the CLI would reject is
// a refusal nobody can act on.
func taskRefArg(t *client.Task) string {
	if t == nil {
		return ""
	}
	if t.Identifier != "" {
		return t.Identifier
	}
	return fmt.Sprintf("%d", t.ID)
}

func taskRef(t *client.Task) string {
	if t == nil {
		return "this task"
	}
	if t.Identifier != "" {
		return t.Identifier
	}
	return fmt.Sprintf("#%d", t.ID)
}

func baseOr(lag *client.TaskLag) string {
	if lag != nil && lag.Base != "" {
		return lag.Base
	}
	return "the integration branch"
}

func remoteOf(base string) string {
	if i := strings.Index(base, "/"); i > 0 {
		return base[:i]
	}
	return "origin"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
