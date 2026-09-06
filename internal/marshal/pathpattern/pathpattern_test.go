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

package pathpattern

import (
	"strings"
	"testing"
)

// The cases below are the server's (pkg/models/path_pattern_test.go and
// TestPathCoveredBy in scope_check_test.go). When one side changes, the other
// must too.

func TestNormalize(t *testing.T) {
	ok := map[string]string{
		"pkg/models/tasks.go":     "pkg/models/tasks.go",
		"./pkg/models/":           "pkg/models",
		"/pkg//models//tasks.go":  "pkg/models/tasks.go",
		"  frontend/src/**  ":     "frontend/src/**",
		"pkg\\models\\tasks.go":   "pkg/models/tasks.go",
		"**":                      "**",
		"pkg/models/*_test.go":    "pkg/models/*_test.go",
		"a/b/c/d/e/f/g/h/i/j/k/l": "a/b/c/d/e/f/g/h/i/j/k/l",
		"./././pkg":               "pkg",
		"api:pkg/models/**":       "api:pkg/models/**",
		"web: ./src//App.vue":     "web:src/App.vue",
		"c:program/x":             "c:program/x",
	}
	for in, want := range ok {
		got, err := Normalize(in)
		if err != nil {
			t.Errorf("Normalize(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{"", "   ", "/", ".", "./", "../etc/passwd", "pkg/../secrets", "pkg/./x", "a\nb", strings.Repeat("x", 501), "api:", "bad repo:pkg/x", "api:../x"}
	for _, in := range bad {
		if _, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) must be rejected", in)
		}
	}
}

// TestCanonical is the contract every scope path is held to, everywhere it is
// written or compared. Canonical is the one function; Normalize is it with no
// repository namespace to apply.
func TestCanonical(t *testing.T) {
	// One table, three implementations. The shell copy of this rule lives in
	// .github/actions/workman-merge-hook/common.sh and is tested against the
	// same cases by canonical-path_test.sh; when one changes, all three do.
	cases := []struct {
		raw, repo, want string
	}{
		// Already canonical — git's own output must survive untouched.
		{"pkg/models/tasks.go", "", "pkg/models/tasks.go"},
		{"captain-yard-web/src/server/db/repo.ts", "", "captain-yard-web/src/server/db/repo.ts"},
		{".github/workflows/test.yml", "", ".github/workflows/test.yml"},
		{"frontend/src/**", "", "frontend/src/**"},

		// Spellings that must collapse onto one identity.
		{"./pkg/models/tasks.go", "", "pkg/models/tasks.go"},
		{"/pkg/models/tasks.go", "", "pkg/models/tasks.go"},
		{".//pkg//models/tasks.go", "", "pkg/models/tasks.go"},
		{"pkg/models/", "", "pkg/models"},
		{"pkg\\models\\tasks.go", "", "pkg/models/tasks.go"},
		{"  frontend/src/**  ", "", "frontend/src/**"},

		// The repository namespace: applied to a bare path, never twice, and
		// never to a path that already names a repository.
		{"pkg/models/**", "api", "api:pkg/models/**"},
		{" docs/x.md ", "api", "api:docs/x.md"},
		{"web:src/App.vue", "api", "web:src/App.vue"},
		{"api:pkg/models/**", "api", "api:pkg/models/**"},
		{"./src//App.vue", "web", "web:src/App.vue"},

		// NOT rebased. "src/server/db/repo.ts" is a different file from
		// "captain-yard-web/src/server/db/repo.ts" and stays one: an app_root
		// is where the application lives, not a base for a claim, and guessing
		// which the caller meant is how a claim lands on a file nobody meant
		// to claim.
		{"src/server/db/repo.ts", "", "src/server/db/repo.ts"},
	}
	for _, c := range cases {
		got, err := Canonical(c.raw, c.repo)
		if err != nil {
			t.Errorf("Canonical(%q, %q): unexpected error %v", c.raw, c.repo, err)
			continue
		}
		if got != c.want {
			t.Errorf("Canonical(%q, %q) = %q, want %q", c.raw, c.repo, got, c.want)
		}
		// Canonicalising a canonical path must be a no-op, or "already stored"
		// and "just written" could differ.
		again, err := Canonical(got, c.repo)
		if err != nil || again != got {
			t.Errorf("Canonical(%q, %q) is not idempotent: %q, %v", got, c.repo, again, err)
		}
	}

	// A path that could escape the repository, name nothing, or carry a
	// namespace that is not a name is refused rather than repaired.
	bad := []struct{ raw, repo string }{
		{"", ""}, {"   ", ""}, {"/", ""}, {".", ""}, {"./", ""},
		{"../etc/passwd", ""}, {"pkg/../secrets", ""}, {"pkg/./x", ""},
		{"a\nb", ""}, {"api:", ""}, {"bad repo:pkg/x", ""}, {"api:../x", ""},
		{"../escape", "api"}, {"", "api"},
	}
	for _, c := range bad {
		if got, err := Canonical(c.raw, c.repo); err == nil {
			t.Errorf("Canonical(%q, %q) = %q, must be rejected", c.raw, c.repo, got)
		}
	}
}

// TestCanonicalAll pins the list form: blanks dropped, duplicates folded once
// they canonicalise to the same identity, order of first appearance kept, and
// one bad entry failing the whole list rather than half-writing a scope.
func TestCanonicalAll(t *testing.T) {
	got, err := CanonicalAll([]string{"./pkg/models//**", "", "  ", "pkg/models/**", "docs/x.md"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pkg/models/**", "docs/x.md"}
	if len(got) != len(want) {
		t.Fatalf("CanonicalAll = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CanonicalAll = %v, want %v", got, want)
		}
	}
	if _, err := CanonicalAll([]string{"pkg/x", "../escape"}, ""); err == nil {
		t.Fatal("one invalid entry must fail the whole list")
	}
}

func TestOverlap(t *testing.T) {
	overlap := [][2]string{
		{"api:pkg/models/**", "api:pkg/models/tasks.go"},
		{"pkg/models/tasks.go", "pkg/models/tasks.go"},
		{"pkg/models", "pkg/models/tasks.go"},
		{"pkg/models/**", "pkg/models/tasks.go"},
		{"pkg/**", "pkg/models/tasks.go"},
		{"**", "anything/at/all.go"},
		{"pkg/models/*.go", "pkg/models/tasks.go"},
		{"pkg/models/*_test.go", "pkg/models/tasks_test.go"},
		{"pkg/models/**", "pkg/models/deep/nested/file.go"},
		{"pkg/models/**", "pkg/models/*.go"},
		{"pkg/**", "pkg/models/**"},
		{"pkg/models/*.go", "pkg/models/*_test.go"},
		{"frontend/src/**/*.vue", "frontend/src/components/Foo.vue"},
		{"frontend/src/**/*.vue", "frontend/src/**"},
		{"pkg/models/ta?ks.go", "pkg/models/tasks.go"},
		{"pkg/models/[tu]asks.go", "pkg/models/tasks.go"},
	}
	for _, c := range overlap {
		if !Overlap(c[0], c[1]) {
			t.Errorf("%q vs %q should overlap", c[0], c[1])
		}
		if !Overlap(c[1], c[0]) {
			t.Errorf("%q vs %q should overlap (symmetric)", c[1], c[0])
		}
	}

	disjoint := [][2]string{
		{"api:pkg/models/**", "web:pkg/models/tasks.go"},
		{"api:src/index.ts", "src/index.ts"},
		{"pkg/models/tasks.go", "pkg/models/labels.go"},
		{"pkg/models/tasks.go", "pkg/models/tasks.go.bak"},
		{"pkg/models/**", "pkg/routes/**"},
		{"pkg/models/*.go", "pkg/models/deep/file.go"},
		{"pkg/models/*.go", "pkg/models/tasks.ts"},
		{"pkg/models/*_test.go", "pkg/models/tasks.go"},
		{"frontend/src/**/*.vue", "frontend/src/components/Foo.ts"},
		{"frontend/**", "pkg/**"},
		{"pkg/models/*.go", "pkg/routes/*.go"},
		{"a/b", "a/bc"},
	}
	for _, c := range disjoint {
		if Overlap(c[0], c[1]) {
			t.Errorf("%q vs %q should be disjoint", c[0], c[1])
		}
		if Overlap(c[1], c[0]) {
			t.Errorf("%q vs %q should be disjoint (symmetric)", c[1], c[0])
		}
	}
}

func TestCovers(t *testing.T) {
	yes := [][2]string{
		{"pkg/models/tasks.go", "pkg/models/tasks.go"},
		{"pkg/models", "pkg/models/tasks.go"},
		{"pkg/models/**", "pkg/models/sub/deep.go"},
		{"pkg/models/*.go", "pkg/models/tasks.go"},
		{"**/*_test.go", "pkg/models/tasks_test.go"},
		{"api:pkg/**", "api:pkg/models/tasks.go"},
	}
	for _, c := range yes {
		if !Covers(c[0], c[1]) {
			t.Errorf("%q should cover %q", c[0], c[1])
		}
	}
	no := [][2]string{
		{"pkg/models/tasks.go", "pkg/models/tasks.go.bak"},
		{"pkg/models/*.go", "pkg/models/sub/deep.go"},
		{"pkg/routes/**", "pkg/models/tasks.go"},
		{"api:pkg/**", "web:pkg/models/tasks.go"},
		{"pkg/**", "api:pkg/models/tasks.go"},
		{"pkg/models/tasks.go", "pkg/models"},
	}
	for _, c := range no {
		if Covers(c[0], c[1]) {
			t.Errorf("%q must not cover %q", c[0], c[1])
		}
	}
}

func TestMatchAny(t *testing.T) {
	patterns := []string{"pkg/models/*.go", "frontend/src/**", "docs/README.md"}
	hit := []string{"pkg/models/tasks.go", "frontend/src/a/b/c.vue", "docs/README.md", "docs/README.md/odd"}
	for _, f := range hit {
		if !MatchAny(patterns, f) {
			t.Errorf("MatchAny should match %q", f)
		}
	}
	miss := []string{"pkg/models/sub/deep.go", "frontend/README.md", "docs/README.mdx", "README.md"}
	for _, f := range miss {
		if MatchAny(patterns, f) {
			t.Errorf("MatchAny must not match %q", f)
		}
	}
	if MatchAny(nil, "anything") {
		t.Error("no patterns must match nothing")
	}
}

func TestSplitRepo(t *testing.T) {
	cases := []struct{ in, repo, rest string }{
		{"api:pkg/models/**", "api", "pkg/models/**"},
		{"pkg/models/**", "", "pkg/models/**"},
		{"c:program/x", "c", "program/x"},
		{":pkg", "", ":pkg"},
		{"pkg/we:ird", "", "pkg/we:ird"},
		{"*:x", "", "*:x"},
		{"bad repo:pkg/x", "bad repo", "pkg/x"},
	}
	for _, c := range cases {
		repo, rest := SplitRepo(c.in)
		if repo != c.repo || rest != c.rest {
			t.Errorf("SplitRepo(%q) = (%q, %q), want (%q, %q)", c.in, repo, rest, c.repo, c.rest)
		}
	}
}
