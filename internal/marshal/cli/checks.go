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
	"strings"

	"github.com/spf13/cobra"

	"code.vikunja.io/veans/internal/marshal/refs"
	"code.vikunja.io/veans/internal/output"
)

func newRefsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refs",
		Short: "Spec references: resolve, check the board, find pastes",
	}
	var rev string
	cmd.PersistentFlags().StringVar(&rev, "rev", "", "git revision to read the spec at (default: working tree)")

	resolve := &cobra.Command{
		Use:   "resolve <ref>...",
		Short: "Resolve FR-nnn / AD-n / NFR-n / D-n to their text, stamped with provenance",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			e.SpecRev = rev
			ix, warnings, err := e.Index(cmd.Context())
			if err != nil {
				return err
			}
			found := refs.Extract(strings.Join(args, " "), e.Prefixes())
			if len(found) == 0 {
				return output.New(output.CodeValidation, "no reference with a configured prefix in %q", strings.Join(args, " "))
			}
			res := ix.Resolve(found)
			exit := error(nil)
			for _, r := range res {
				if !r.Found {
					exit = output.New(output.CodeNotFound, "%s does not resolve at %s", r.Ref.ID, ixRev(ix))
				}
			}
			if err := emit(cmd.OutOrStdout(), map[string]any{"rev": ix.Rev, "resolutions": res, "warnings": warnings}); err != nil {
				return err
			}
			return exit
		},
	}

	check := &cobra.Command{
		Use:   "check [task]",
		Short: "Broken references, verbatim spec pastes and unlinked tasks across the board (or one task)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			e.SpecRev = rev
			if len(args) == 1 {
				t, err := resolveTask(cmd.Context(), e, args[0])
				if err != nil {
					return err
				}
				res, err := e.ResolveTask(cmd.Context(), t)
				if err != nil {
					return err
				}
				if err := emit(cmd.OutOrStdout(), res); err != nil {
					return err
				}
				if res.Broken > 0 || len(res.Pastes) > 0 {
					return output.New(output.CodeConflict, "%d broken reference(s), %d paste(s)", res.Broken, len(res.Pastes))
				}
				return nil
			}
			snap, err := e.Board.Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			rep, err := e.References(cmd.Context(), snap)
			if err != nil {
				return err
			}
			if err := emit(cmd.OutOrStdout(), rep); err != nil {
				return err
			}
			if len(rep.Broken) > 0 || len(rep.Pastes) > 0 {
				return output.New(output.CodeConflict, "%d broken reference(s), %d paste(s)", len(rep.Broken), len(rep.Pastes))
			}
			return nil
		},
	}

	index := &cobra.Command{
		Use:   "index",
		Short: "List every anchor the configured sources define",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			e.SpecRev = rev
			ix, warnings, err := e.Index(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), map[string]any{"rev": ix.Rev, "anchors": ix.Anchors, "files": ix.Files, "warnings": warnings})
		},
	}
	cmd.AddCommand(resolve, check, index)
	return cmd
}

func ixRev(ix *refs.Index) string {
	if ix.Rev == "" {
		return "the working tree"
	}
	return ix.Rev
}

func newHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "The graph invariants: every story claims files, no unordered overlap, no blocked cycle",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			snap, err := e.Board.Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			h := e.Health(cmd.Context(), snap)
			if err := emit(cmd.OutOrStdout(), h); err != nil {
				return err
			}
			if !h.OK {
				return output.New(output.CodeConflict, "%d collision(s), %d cycle(s), %d finding(s)", h.Collisions, h.Cycles, len(h.Findings))
			}
			return nil
		},
	}
}

func newChokepointsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chokepoints",
		Short: "The CODEOWNERS files and who is queued on each",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			snap, err := e.Board.Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			rep, err := e.Chokepoints(snap)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), rep)
		},
	}
}

func newOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <path>",
		Short: "Is anything open on this path? Declared scopes and live leases that cover it",
		Long: `Prints every open task whose declared scope or live lease covers the path,
with whether the lease is actually held right now.

Paths are canonical: relative to the REPOSITORY root, forward slashes, no
leading "/", no "./", no ".." and no trailing slash. When the app lives in a
sub-directory the sub-directory is part of the path — an app in
captain-yard-web/ claims "captain-yard-web/src/db/schema.ts", never
"src/db/schema.ts". Globs: * matches within a segment, ** across segments;
a bare directory such as pkg/models covers its whole subtree. In a project
spanning several repositories a "repo:" prefix comes first, and .veans.yml's
repository adds it to bare paths for you.

The repository root is the base because that is what git prints, and a lease
is exclusion enforced by comparing strings: two spellings of one file are two
claims that cannot see each other.

A path spelled from the wrong base is not an error — it is a different file,
and the honest answer for a file nobody claims is an empty holder list. If a
lookup comes back empty for a file you know is claimed, check the base before
concluding it is free.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			snap, err := e.Board.Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			holders, err := e.OpenOnPath(snap, args[0])
			if err != nil {
				return err
			}
			// Echo the path actually queried, not the one typed: a lookup that
			// answers about a path you did not write is the one answer you
			// most need to see.
			queried, err := e.CanonicalPath(args[0])
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), map[string]any{"path": queried, "holders": holders})
		},
	}
}

func newLagCmd() *cobra.Command {
	var base string
	cmd := &cobra.Command{
		Use:     "lag",
		Aliases: []string{"rebase-plan"},
		Short:   "How far each claimed branch is behind the integration branch, in the files it holds",
		Long: `Measures every claimed branch against the integration branch and reports
the paths it holds that have moved since it diverged, with which task landed
each one.

Severity is scoped to the claim: owned means the base moved inside a file the
task said it would edit, so a conflict is certain; affected means it moved in
something the task reads but does not edit. Commits behind is context for that
list, not a number to drive to zero.

Read-only. It computes the answer rather than reading back what the last poll
stored, so it is current even between polls, and it touches no worktree.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			snap, err := e.Board.Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			if base == "" {
				base = e.Cfg.IntegrationBranch
			}
			rep, err := e.ComputeLag(cmd.Context(), snap, base)
			if err != nil {
				return err
			}
			if err := emit(cmd.OutOrStdout(), rep); err != nil {
				return err
			}
			blocking := 0
			for _, b := range rep.Branches {
				if b.Lag.Blocking() {
					blocking++
				}
			}
			if blocking > 0 {
				return output.New(output.CodeConflict, "%d branch(es) are behind %s in a file they own", blocking, rep.Base)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "integration branch to measure against (default: .marshal.yml's integration_branch)")
	return cmd
}

func newReconcileCmd() *cobra.Command {
	var base, branch string
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Validate every claimed task's branch against its claim; flag quiet branches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			snap, err := e.Board.Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			if base == "" {
				base = e.Cfg.IntegrationBranch
			}
			checks, err := e.Reconcile(cmd.Context(), snap, base, branch)
			if err != nil {
				return err
			}
			if err := emit(cmd.OutOrStdout(), map[string]any{"base": base, "branches": checks}); err != nil {
				return err
			}
			bad := 0
			for _, c := range checks {
				if c.Stale || (c.Result != nil && !c.Result.OK) {
					bad++
				}
			}
			if bad > 0 {
				return output.New(output.CodeConflict, "%d branch(es) stale or outside their claim", bad)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "integration branch to diff against (default: .marshal.yml's integration_branch)")
	cmd.Flags().StringVar(&branch, "branch", "", "only this branch")
	return cmd
}
