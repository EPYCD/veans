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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// lagRepo builds the shape the whole feature is about: a branch cut from main,
// and main then moving on underneath it.
//
//	main:    A --- B(schema.ts, "Refs: CY-43") --- C(readme)
//	          \
//	feature:   D(mine.ts)
func lagRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_AUTHOR_DATE=2026-09-05T11:02:00+00:00",
			"GIT_COMMITTER_DATE=2026-09-05T11:02:00+00:00",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "--initial-branch=main")
	write("README", "a")
	run("add", ".")
	run("commit", "-m", "A")

	run("checkout", "-b", "feature")
	write("app/src/mine.ts", "mine")
	run("add", ".")
	run("commit", "-m", "D")

	run("checkout", "main")
	write("app/src/server/db/schema.ts", "landed")
	run("add", ".")
	run("commit", "-m", "B: the document model\n\nRefs: CY-43")
	write("README", "b")
	run("add", ".")
	run("commit", "-m", "C: readme only, no trailer")
	run("checkout", "feature")
	return root
}

func TestLagWalk(t *testing.T) {
	repo := lagRepo(t)
	ctx := context.Background()

	mergeBase, err := MergeBase(ctx, repo, "main", "feature")
	if err != nil || mergeBase == "" {
		t.Fatalf("MergeBase = %q, %v", mergeBase, err)
	}

	// Behind counts what main has and feature does not: B and C. The commit
	// feature has and main does not must NOT be counted — being ahead is not
	// being behind.
	behind, err := CommitsBehind(ctx, repo, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if behind != 2 {
		t.Errorf("CommitsBehind = %d, want 2", behind)
	}

	baseTip, err := ResolveSHA(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	moved, err := MovedFiles(ctx, repo, mergeBase, baseTip)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(moved)
	if want := []string{"README", "app/src/server/db/schema.ts"}; !reflect.DeepEqual(moved, want) {
		t.Errorf("MovedFiles = %v, want %v", moved, want)
	}
	// The file only the feature branch touched is not something main moved.
	if slices.Contains(moved, "app/src/mine.ts") {
		t.Error("MovedFiles must diff the base's own range, not the symmetric difference")
	}

	landing, err := LastLanding(ctx, repo, mergeBase, baseTip, "app/src/server/db/schema.ts")
	if err != nil || landing == nil {
		t.Fatalf("LastLanding = %v, %v", landing, err)
	}
	if !reflect.DeepEqual(landing.Refs, []string{"CY-43"}) {
		t.Errorf("Refs = %v, want [CY-43]", landing.Refs)
	}
	if landing.SHA == "" || landing.At.IsZero() {
		t.Errorf("landing = %+v", landing)
	}

	// A commit with no trailer is still a landing: the sha stands on its own.
	readme, err := LastLanding(ctx, repo, mergeBase, baseTip, "README")
	if err != nil || readme == nil {
		t.Fatalf("LastLanding(README) = %v, %v", readme, err)
	}
	if len(readme.Refs) != 0 {
		t.Errorf("a commit with no trailer must claim no task: %v", readme.Refs)
	}

	// A path nothing touched has no landing at all.
	none, err := LastLanding(ctx, repo, mergeBase, baseTip, "app/src/mine.ts")
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Errorf("LastLanding for an untouched path = %+v", none)
	}
}

func TestCommitsBehindWhenCurrent(t *testing.T) {
	repo := lagRepo(t)
	ctx := context.Background()
	// main is not behind itself.
	behind, err := CommitsBehind(ctx, repo, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	if behind != 0 {
		t.Errorf("CommitsBehind(main, main) = %d, want 0", behind)
	}
}

func TestRefsTrailers(t *testing.T) {
	cases := map[string][]string{
		"feat: x\n\nRefs: CY-43":            {"CY-43"},
		"feat: x\n\nrefs: #12, 13 CY-14":    {"#12", "13", "CY-14"},
		"feat: x\n\nRef: CY-1":              {"CY-1"},
		"feat: x\n\nCo-Authored-By: nobody": nil,
		"":                                  nil,
		// Duplicates across two trailers fold into one.
		"x\n\nRefs: CY-1\nRefs: CY-1": {"CY-1"},
	}
	for msg, want := range cases {
		got := RefsTrailers(msg)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("RefsTrailers(%q) = %v, want %v", msg, got, want)
		}
	}
}
