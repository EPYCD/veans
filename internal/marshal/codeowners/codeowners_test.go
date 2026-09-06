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

package codeowners

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"code.vikunja.io/veans/internal/marshal/pathpattern"
)

func loadCaptainYard(t *testing.T) *File {
	t.Helper()
	b, err := os.ReadFile("testdata/CODEOWNERS")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func patterns(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Pattern)
	}
	return out
}

func TestParseCaptainYard(t *testing.T) {
	f := loadCaptainYard(t)

	want := []struct {
		pattern string
		line    int
	}{
		{"/captain-yard-web/src/lib/contract.ts", 16},
		{"/captain-yard-web/src/lib/contract-routes.ts", 17},
		{"/captain-yard-web/src/server/db/schema.ts", 22},
		{"/captain-yard-web/src/server/db/repo.ts", 28},
		{"/captain-yard-web/src/server/db/indexes.ts", 29},
		{"/captain-yard-web/src/server/auth/", 30},
		{"/captain-yard-web/packages/engine/", 34},
		{"/.github/", 37},
	}
	if len(f.Rules) != len(want) {
		t.Fatalf("got %d rules, want %d: %v", len(f.Rules), len(want), patterns(f.Rules))
	}
	for i, w := range want {
		r := f.Rules[i]
		if r.Pattern != w.pattern || r.Line != w.line {
			t.Errorf("rule %d = %q@%d, want %q@%d", i, r.Pattern, r.Line, w.pattern, w.line)
		}
		if !reflect.DeepEqual(r.Owners, []string{"@subinsayzz"}) {
			t.Errorf("rule %d owners = %v", i, r.Owners)
		}
	}

	// The file-level header is separated by a blank line and must not leak
	// into the first rule's comment.
	contract := f.Rules[0].Comment
	if !strings.HasPrefix(contract, "── The contract: one 7,000+ line object literal") {
		t.Errorf("contract comment starts wrong: %q", contract)
	}
	if strings.Contains(contract, "Purpose:") {
		t.Errorf("file header leaked into rule comment: %q", contract)
	}
	if !strings.HasSuffix(contract, "fails in three places at once.") {
		t.Errorf("contract comment ends wrong: %q", contract)
	}
	if f.Rules[1].Comment != contract {
		t.Errorf("consecutive rule should share the block: %q", f.Rules[1].Comment)
	}
	if got := f.Rules[2].Comment; !strings.HasPrefix(got, "── The document model ──") {
		t.Errorf("schema comment = %q", got)
	}
	for _, i := range []int{3, 4, 5} {
		if got := f.Rules[i].Comment; !strings.HasPrefix(got, "── Tenant isolation ──") || !strings.Contains(got, "security model") {
			t.Errorf("rule %d comment = %q", i, got)
		}
	}
	if got := f.Rules[6].Comment; !strings.Contains(got, "purity test") {
		t.Errorf("engine comment = %q", got)
	}
	if got := f.Rules[7].Comment; got != "── The gates themselves ──" {
		t.Errorf(".github comment = %q", got)
	}
}

func TestMatchesCaptainYard(t *testing.T) {
	f := loadCaptainYard(t)
	cases := map[string][]string{
		// contract.ts is a file rule; a directory of a different name beside it is not covered.
		"captain-yard-web/src/lib/contract/hire.ts":    nil,
		"captain-yard-web/src/lib/contract.ts":         {"/captain-yard-web/src/lib/contract.ts"},
		"captain-yard-web/packages/engine/x.ts":        {"/captain-yard-web/packages/engine/"},
		"captain-yard-web/packages/engine/deep/y.ts":   {"/captain-yard-web/packages/engine/"},
		"captain-yard-web/src/server/auth/session.ts":  {"/captain-yard-web/src/server/auth/"},
		"/captain-yard-web/src/server/db/repo.ts":      {"/captain-yard-web/src/server/db/repo.ts"},
		"./captain-yard-web/src/server/db/indexes.ts":  {"/captain-yard-web/src/server/db/indexes.ts"},
		".github/workflows/ci.yml":                     {"/.github/"},
		"README.md":                                    nil,
		"captain-yard-web/src/server/db/repo.test.ts":  nil,
		"other/captain-yard-web/src/lib/contract.ts":   nil,
		"captain-yard-web/packages/engine-extras/x.ts": nil,
		"": nil,
	}
	for path, want := range cases {
		got := patterns(f.Matches(path))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Matches(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestChokepointsCaptainYard(t *testing.T) {
	f := loadCaptainYard(t)

	// Canonical form: repository-root-relative, exactly as the board holds a
	// claim. app_root is not a base for a claim and never rebases these.
	want := []string{
		"captain-yard-web/src/lib/contract.ts",
		"captain-yard-web/src/lib/contract-routes.ts",
		"captain-yard-web/src/server/db/schema.ts",
		"captain-yard-web/src/server/db/repo.ts",
		"captain-yard-web/src/server/db/indexes.ts",
		"captain-yard-web/src/server/auth/**",
		"captain-yard-web/packages/engine/**",
		".github/**",
	}
	if got := f.Chokepoints(); !reflect.DeepEqual(got, want) {
		t.Errorf("Chokepoints() = %v, want %v", got, want)
	}
}

// TestChokepointsAreClaimable is the invariant whose absence let an
// always-empty queue ship: every chokepoint must live in the same namespace as
// a stored claim, so a lease on the file a chokepoint names can be seen from
// the chokepoint. Before app_root stopped being stripped, every one of these
// failed on a project that set one.
func TestChokepointsAreClaimable(t *testing.T) {
	f := loadCaptainYard(t)
	// What the pre-commit hook accepts, because git prints it: a path from the
	// repository root.
	leased := "captain-yard-web/src/server/db/repo.ts"
	for _, cp := range f.Chokepoints() {
		if _, err := pathpattern.Normalize(cp); err != nil {
			t.Errorf("chokepoint %q is not a valid scope path: %v", cp, err)
		}
	}
	var reached bool
	for _, cp := range f.Chokepoints() {
		if pathpattern.Overlap(cp, leased) {
			reached = true
		}
	}
	if !reached {
		t.Fatalf("no chokepoint overlaps a live claim on %q — the queue for it can never be non-empty", leased)
	}
}

const gitHubShapes = `# Docs
*.md @docs
docs/* @docs2
/build/logs/ @ci
apps/ @apps
/**/logs @logs
src/*/lib @lib
foo @x # trailing comment
/apps/github
`

func TestMatchesGitHubSemantics(t *testing.T) {
	f, err := Parse([]byte(gitHubShapes))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rules) != 8 {
		t.Fatalf("got %d rules", len(f.Rules))
	}
	if got := f.Rules[6].Owners; !reflect.DeepEqual(got, []string{"@x"}) {
		t.Errorf("trailing comment must not become an owner: %v", got)
	}
	if got := f.Rules[7].Owners; got != nil {
		t.Errorf("owner-less rule should have no owners: %v", got)
	}
	if got := f.Rules[0].Comment; got != "Docs" {
		t.Errorf("comment = %q", got)
	}

	cases := map[string][]string{
		"README.md":               {"*.md"},
		"docs/getting-started.md": {"docs/*", "*.md"},
		// GitHub's documented deviation from gitignore: docs/* does not descend.
		"docs/build-app/troubleshooting.md": {"*.md"},
		"build/logs/x.txt":                  {"/**/logs", "/build/logs/"},
		"deeply/nested/logs/y":              {"/**/logs"},
		"x/apps/y.go":                       {"apps/"},
		"apps/github/z.go":                  {"/apps/github", "apps/"},
		"apps/other/z.go":                   {"apps/"},
		"src/a/lib/x.ts":                    {"src/*/lib"},
		"src/a/b/lib/x.ts":                  nil,
		"a/foo":                             {"foo"},
		"a/foo/b":                           {"foo"},
		"a/foobar":                          nil,
	}
	for path, want := range cases {
		got := patterns(f.Matches(path))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Matches(%q) = %v, want %v", path, got, want)
		}
	}

	want := []string{"**/*.md", "docs/*", "build/logs/**", "**/apps/**", "**/logs", "src/*/lib", "**/foo", "apps/github"}
	if got := f.Chokepoints(); !reflect.DeepEqual(got, want) {
		t.Errorf("Chokepoints() = %v, want %v", got, want)
	}
}

func TestChokepointsDistinct(t *testing.T) {
	// "a/b/" has a middle slash, so gitignore anchors it: same chokepoint as "/a/b/".
	f, err := Parse([]byte("/a/b/ @x\n/a/b/ @y\na/b/ @z\nb/ @w\n/ @root\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b/**", "**/b/**", "**"}; !reflect.DeepEqual(f.Chokepoints(), want) {
		t.Errorf("Chokepoints() = %v, want %v", f.Chokepoints(), want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"!foo @x\n", "../escape @x\n", "a/./b @x\n"} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
	f, err := Parse([]byte("\n# only comments\n\n"))
	if err != nil || len(f.Rules) != 0 {
		t.Errorf("comment-only file: rules=%v err=%v", f.Rules, err)
	}
}

// TestChokepointQueueContainsTheLeaseHolder is regression test 3 of the brief,
// end to end: a CODEOWNERS entry "/app/src/x.ts" and a live lease on
// "app/src/x.ts" must put that task on the chokepoint's queue.
//
// It would have failed from the day the feature shipped until the day
// app_root stopped being stripped, on every project that sets one. Nothing
// noticed, because a queue with no rows in it and a queue with nothing to
// report render identically — which is the whole reason the reachability
// invariant now exists rather than being left as a follow-up.
func TestChokepointQueueContainsTheLeaseHolder(t *testing.T) {
	f, err := Parse([]byte("/app/src/x.ts @owner\n/app/src/lib/ @owner\n"))
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)
	claims := []Claim{
		// Spelled the only way the pre-commit hook accepts: from the
		// repository root, exactly as git prints it.
		{TaskID: 46, Identifier: "#46", Title: "The write door", Pattern: "app/src/x.ts", Since: since, Active: true},
		{TaskID: 51, Identifier: "#51", Title: "The depot-pinned login", Pattern: "app/src/lib/contract.ts", Since: since},
	}

	queues := Queues(f.Chokepoints(), claims)
	if len(queues) != 2 {
		t.Fatalf("got %d queues, want 2", len(queues))
	}

	if queues[0].Chokepoint != "app/src/x.ts" {
		t.Errorf("chokepoint = %q, want the repository-root-relative form", queues[0].Chokepoint)
	}
	if len(queues[0].Entries) != 1 {
		t.Fatalf("the queue for a file with a live lease on it must not be empty: %+v", queues[0])
	}
	got := queues[0].Entries[0]
	if got.Claim.TaskID != 46 || !got.Claim.Active || got.Position != 1 {
		t.Errorf("entry = %+v, want #46 holding position 1", got)
	}

	// The directory rule reaches the file below it, in the same namespace.
	if len(queues[1].Entries) != 1 || queues[1].Entries[0].Claim.TaskID != 51 {
		t.Errorf("queue for %q = %+v", queues[1].Chokepoint, queues[1].Entries)
	}
}

func TestQueues(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	claims := []Claim{
		// The holder started last but holds the lease now.
		{TaskID: 1, Identifier: "CY-1", Pattern: "src/lib/contract.ts", Since: t0.Add(2 * time.Hour), Active: true, Assignee: "Ada"},
		// The same task's declared claim must fold into its live one.
		{TaskID: 1, Identifier: "CY-1", Pattern: "src/lib/contract.ts", Since: t0.Add(-time.Hour)},
		{TaskID: 2, Identifier: "CY-2", Pattern: "src/lib/**", Since: t0, BlockedBy: []int64{3}},
		{TaskID: 3, Identifier: "CY-3", Pattern: "src/lib/contract.ts", Since: t0.Add(time.Hour)},
		{TaskID: 4, Identifier: "CY-4", Pattern: "src/lib/contract.ts", Since: t0.Add(time.Hour), BlockedBy: []int64{99}},
		{TaskID: 5, Identifier: "CY-5", Pattern: "src/server/db/schema.ts", Since: t0},
	}
	queues := Queues([]string{"src/lib/contract.ts", "src/server/db/schema.ts", "packages/engine/**"}, claims)
	if len(queues) != 3 {
		t.Fatalf("got %d queues", len(queues))
	}

	type row struct {
		pos     int
		task    int64
		active  bool
		waiting []int64
	}
	rows := func(q Queue) []row {
		out := make([]row, len(q.Entries))
		for i, e := range q.Entries {
			out[i] = row{e.Position, e.Claim.TaskID, e.Claim.Active, e.WaitingOn}
		}
		return out
	}

	// CY-2 declared first but is blocked by CY-3, so CY-3 goes ahead of it;
	// CY-4 ties with CY-3 on Since and loses on id.
	want := []row{
		{1, 1, true, nil},
		{2, 3, false, []int64{1}},
		{3, 2, false, []int64{1, 3}},
		{4, 4, false, []int64{1}},
	}
	if got := rows(queues[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("contract queue = %+v, want %+v", got, want)
	}
	if queues[0].Entries[0].Claim.Assignee != "Ada" {
		t.Errorf("holder entry lost its live claim: %+v", queues[0].Entries[0].Claim)
	}
	if got := rows(queues[1]); !reflect.DeepEqual(got, []row{{1, 5, false, nil}}) {
		t.Errorf("schema queue = %+v", got)
	}
	if queues[2].Chokepoint != "packages/engine/**" || len(queues[2].Entries) != 0 {
		t.Errorf("unclaimed chokepoint = %+v", queues[2])
	}
}

func TestQueuesMultipleHoldersAndCycle(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	claims := []Claim{
		{TaskID: 7, Pattern: "src/server/db/repo.ts", Since: t0.Add(time.Minute), Active: true},
		{TaskID: 8, Pattern: "src/server/db/indexes.ts", Since: t0, Active: true},
		{TaskID: 9, Pattern: "src/server/db/**", Since: t0, BlockedBy: []int64{10}},
		{TaskID: 10, Pattern: "src/server/db/schema.ts", Since: t0.Add(time.Minute), BlockedBy: []int64{9}},
	}
	q := Queues([]string{"src/server/db/**"}, claims)[0]
	var order []int64
	for _, e := range q.Entries {
		order = append(order, e.Claim.TaskID)
	}
	if want := []int64{8, 7, 9, 10}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if got := q.Entries[1].WaitingOn; !reflect.DeepEqual(got, []int64{8}) {
		t.Errorf("second holder waits on %v", got)
	}
	if got := q.Entries[3].WaitingOn; !reflect.DeepEqual(got, []int64{7, 8, 9}) {
		t.Errorf("cycle member waits on %v", got)
	}
}
