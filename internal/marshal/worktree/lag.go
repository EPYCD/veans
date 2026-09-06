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

package worktree

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Landing is the commit on the integration branch that last touched a path,
// with whatever the Refs: trailer says landed it.
type Landing struct {
	SHA  string
	At   time.Time
	Refs []string
}

// MergeBase returns where branch diverged from base, or "" when they share no
// history (an unrelated branch, which is a state to report rather than crash
// on).
func MergeBase(ctx context.Context, repoRoot, base, branch string) (string, error) {
	out, err := git(ctx, repoRoot, "merge-base", base, branch)
	if err != nil {
		// No merge base is an exit status, not a failure of the command.
		return "", nil //nolint:nilerr // an unrelated branch has no merge base
	}
	return strings.TrimSpace(out), nil
}

// CommitsBehind counts the commits base has that branch does not.
//
// It is context for the collision list, not a number anyone should be asked to
// drive to zero: forty commits behind in files you never touch is noise, one
// commit behind in the file you are editing is certain rework.
func CommitsBehind(ctx context.Context, repoRoot, base, branch string) (int, error) {
	out, err := git(ctx, repoRoot, "rev-list", "--left-right", "--count", base+"..."+branch)
	if err != nil {
		return 0, fmt.Errorf("count commits behind %s: %w", base, err)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	behind, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("parse commits behind from %q: %w", out, err)
	}
	return behind, nil
}

// MovedFiles lists the paths the integration branch changed since the branch
// diverged: `git diff --name-only <merge base>..<base tip>`.
//
// A diff of two commits, not a walk of per-file history — the whole set comes
// back in one command whatever the number of commits, which is what keeps this
// affordable on every poll.
func MovedFiles(ctx context.Context, repoRoot, mergeBase, baseTip string) ([]string, error) {
	out, err := git(ctx, repoRoot, "diff", "--name-only", "--diff-filter=ACMRD", "-z", mergeBase+".."+baseTip, "--")
	if err != nil {
		return nil, fmt.Errorf("list files moved on the integration branch: %w", err)
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// LastLanding returns the newest commit in mergeBase..baseTip that touched
// path, with the task references from its Refs: trailer.
//
// The trailer is how a collision names a task rather than a sha. A commit that
// carries none leaves the refs empty and keeps the sha: a hand-pushed commit
// is still lag, and reporting it as "something landed here" beats not
// reporting it at all.
func LastLanding(ctx context.Context, repoRoot, mergeBase, baseTip, path string) (*Landing, error) {
	const sep = "\x1e"
	out, err := git(ctx, repoRoot, "log", "-1", "--format=%H"+sep+"%aI"+sep+"%B",
		mergeBase+".."+baseTip, "--", path)
	if err != nil {
		return nil, fmt.Errorf("find what landed %s: %w", path, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	parts := strings.SplitN(out, sep, 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("unexpected log output for %s", path)
	}
	l := &Landing{SHA: strings.TrimSpace(parts[0])}
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1])); err == nil {
		l.At = at
	}
	if len(parts) == 3 {
		l.Refs = RefsTrailers(parts[2])
	}
	return l, nil
}

// RefsTrailers reads the task references out of a commit message's Refs:
// trailer, in the same shapes the merge hook accepts: `Refs: PROJ-12`,
// `Refs: #12`, `Refs: 12`, and comma or space separated lists.
func RefsTrailers(message string) []string {
	seen := map[string]bool{}
	var refs []string
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, "refs:") && !strings.HasPrefix(lower, "ref:") {
			continue
		}
		rest := strings.TrimSpace(line[strings.Index(line, ":")+1:])
		for _, tok := range strings.FieldsFunc(rest, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		}) {
			if tok != "" && !seen[tok] {
				seen[tok] = true
				refs = append(refs, tok)
			}
		}
	}
	return refs
}
