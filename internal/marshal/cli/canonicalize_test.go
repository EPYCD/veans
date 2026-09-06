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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"code.vikunja.io/veans/internal/marshal/pathpattern"
)

// repoWith builds a checkout containing the given files, so the rebase rule
// has a repository to consult. It consults one on purpose: the difference
// between a safe rewrite and a claim on a file nobody typed is whether the
// file is actually there.
func repoWith(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCanonicalizeListRebasesOnlyWhenTheRepositorySettlesIt(t *testing.T) {
	roots := pathpattern.ParseRoots("app,docs", "app", "src")
	root := repoWith(t, "app/src/x.ts", "app/src/y.ts", "docs/api.md", "src/ambiguous.ts", "app/src/ambiguous.ts")

	t.Run("an app-relative path whose rebased form exists is rewritten", func(t *testing.T) {
		row := canonicalRow{}
		got, changed, _ := canonicalizeList([]string{"src/x.ts"}, "", roots, root, &row)
		if !reflect.DeepEqual(got, []string{"app/src/x.ts"}) || !changed {
			t.Fatalf("got %v changed=%v", got, changed)
		}
		if len(row.Unresolved) != 0 {
			t.Fatalf("unresolved = %v", row.Unresolved)
		}
	})

	t.Run("a path that exists at BOTH bases is never guessed at", func(t *testing.T) {
		// "src/ambiguous.ts" is a real file at the repository root and
		// "app/src/ambiguous.ts" is a real file too. Rewriting would move the
		// claim onto a different file that happens to share a suffix.
		row := canonicalRow{}
		got, _, _ := canonicalizeList([]string{"src/ambiguous.ts"}, "", roots, root, &row)
		if !reflect.DeepEqual(got, []string{"src/ambiguous.ts"}) {
			t.Fatalf("got %v, want the path left alone", got)
		}
		if !reflect.DeepEqual(row.Unresolved, []string{"src/ambiguous.ts"}) {
			t.Fatalf("unresolved = %v", row.Unresolved)
		}
	})

	t.Run("a path that exists at neither base is left for a human", func(t *testing.T) {
		row := canonicalRow{}
		got, _, _ := canonicalizeList([]string{"src/gone.ts"}, "", roots, root, &row)
		if !reflect.DeepEqual(got, []string{"src/gone.ts"}) || len(row.Unresolved) != 1 {
			t.Fatalf("got %v unresolved=%v", got, row.Unresolved)
		}
	})

	t.Run("a glob names no single file, so the repository cannot settle it", func(t *testing.T) {
		row := canonicalRow{}
		got, _, _ := canonicalizeList([]string{"src/**"}, "", roots, root, &row)
		if !reflect.DeepEqual(got, []string{"src/**"}) || len(row.Unresolved) != 1 {
			t.Fatalf("got %v unresolved=%v", got, row.Unresolved)
		}
	})

	t.Run("spelling alone is tidied without consulting anything", func(t *testing.T) {
		row := canonicalRow{}
		got, changed, _ := canonicalizeList([]string{"./app/src//y.ts", "app/src/y.ts"}, "", roots, root, &row)
		// Both spellings are one file, stored once.
		if !reflect.DeepEqual(got, []string{"app/src/y.ts"}) || !changed {
			t.Fatalf("got %v changed=%v", got, changed)
		}
	})
}

func TestFindCollisionsOnlyReportsWhatCanonicalisationRevealed(t *testing.T) {
	t.Run("two tasks arriving from different spellings is the finding", func(t *testing.T) {
		owner := map[string]map[int64]string{
			"app/src/x.ts": {43: "app/src/x.ts", 51: "src/x.ts"},
		}
		got := findCollisions(owner)
		if len(got) != 1 {
			t.Fatalf("got %d collisions, want 1: %+v", len(got), got)
		}
		if !reflect.DeepEqual(got[0].TaskIDs, []int64{43, 51}) {
			t.Errorf("task ids = %v", got[0].TaskIDs)
		}
		if got[0].Was["51"] != "src/x.ts" {
			t.Errorf("the report must say how each side spelled it: %v", got[0].Was)
		}
	})

	t.Run("two tasks that already claimed the identical string are not this", func(t *testing.T) {
		// Nothing was hidden: they have always been visibly on one file, and
		// the unordered-overlap invariant reports them. Repeating it here
		// would bury the real finding and make a clean board look dirty.
		owner := map[string]map[int64]string{
			"app/src/x.ts": {67: "app/src/x.ts", 83: "app/src/x.ts"},
		}
		if got := findCollisions(owner); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})

	t.Run("one task is never a collision with itself", func(t *testing.T) {
		owner := map[string]map[int64]string{"app/src/x.ts": {43: "src/x.ts"}}
		if got := findCollisions(owner); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})
}
