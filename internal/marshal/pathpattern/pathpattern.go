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

// Package pathpattern is a port of the server's scope-path semantics
// (pkg/models/path_pattern.go) so Marshal judges coverage and overlap exactly
// as the board does. Keep the two in lockstep.
//
// Scope paths are repository-relative glob patterns: `pkg/models/tasks.go`,
// `pkg/models/*.go`, `frontend/src/**`. Segments are matched with path.Match
// and `**` matches any number of segments, which is the subset of glob syntax
// agents actually write and the one whose overlap can be decided cheaply.
//
// # The canonical form
//
// One spelling per file, because a lease is exclusion and exclusion compares
// strings. A scope path is canonical when it is:
//
//   - relative to the REPOSITORY root, never to a sub-directory of it — an app
//     that lives in `captain-yard-web/` claims `captain-yard-web/src/x.ts`;
//   - separated by forward slashes, with no leading `/`, no `./`, no repeated
//     slashes and no `.` or `..` segment;
//   - without a trailing slash: a directory claims its subtree either bare
//     (`pkg/models`) or with an explicit glob (`pkg/models/**`);
//   - prefixed with `repo:` in a project spanning several repositories.
//
// The repository root is the base because git already reports changed files
// that way and cannot be argued with. Every ingress and every comparison in
// veans and Marshal goes through Canonical; two spellings of one file are two
// identities to a lease, an overlap check and a chokepoint queue alike.
package pathpattern

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

const maxLength = 500

// Canonical is the single normalisation every scope path passes through, on
// its way in and before any comparison. It applies repo as the `repo:`
// namespace of a bare pattern (empty for a single-repository project, and
// never applied twice), then canonicalises: trims, folds backslashes to `/`,
// strips a leading `./` or `/`, collapses repeated slashes and drops a
// trailing slash. It rejects anything that could escape the repository.
//
// It does not rebase a path. A path relative to the app sub-directory comes
// back canonically spelled and still relative to the wrong base — an app_root
// is where the application lives, never a base for a claim.
func Canonical(raw, repo string) (string, error) {
	p := strings.TrimSpace(raw)
	if repo != "" && p != "" {
		if existing, _ := SplitRepo(p); existing == "" {
			p = repo + ":" + p
		}
	}
	return normalize(p, raw)
}

// CanonicalAll canonicalises a list, dropping blanks and duplicates while
// keeping the first occurrence's order. It fails on the first bad entry so a
// caller never half-writes a scope.
func CanonicalAll(raw []string, repo string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, r := range raw {
		if strings.TrimSpace(r) == "" {
			continue
		}
		p, err := Canonical(r, repo)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// CanonicalFiles canonicalises paths that came from git, which prints them
// relative to the repository root — the canonical base by definition, so in
// practice this only tidies separators. It is best-effort on purpose: a path
// git printed cannot be invalid in a way that matters to a scope check, and
// dropping a changed file to be strict about it would turn a cosmetic problem
// into a missed collision. The point is that the git side and the claim side
// of a check reach the comparison through the same function.
func CanonicalFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		c, err := Canonical(f, "")
		if err != nil {
			c = f
		}
		out = append(out, c)
	}
	return out
}

// Normalize canonicalises a pattern that already carries whatever `repo:`
// prefix it should have. It is Canonical with no namespace to apply.
func Normalize(raw string) (string, error) {
	return Canonical(raw, "")
}

// normalize does the work. raw is the string the caller passed, echoed back in
// errors so a prefixed rewrite never shows up in a message about a path the
// caller never wrote.
func normalize(p, raw string) (string, error) {
	if p == "" {
		return "", errors.New("invalid scope path: empty")
	}
	if len(p) > maxLength {
		return "", fmt.Errorf("invalid scope path %q: longer than %d characters", raw, maxLength)
	}
	if strings.ContainsAny(p, "\x00\n\r") {
		return "", fmt.Errorf("invalid scope path %q: contains control characters", raw)
	}
	repo, p := SplitRepo(p)
	if repo != "" && !validRepoName(repo) {
		return "", fmt.Errorf("invalid scope path %q: repository prefix may only contain letters, digits, '.', '_' and '-'", raw)
	}
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	p = strings.TrimPrefix(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "." {
		return "", fmt.Errorf("invalid scope path %q: does not name anything", raw)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid scope path %q: must not contain '..'", raw)
		}
		if seg == "." {
			return "", fmt.Errorf("invalid scope path %q: must not contain '.' segments", raw)
		}
	}
	if repo != "" {
		return repo + ":" + p, nil
	}
	return p, nil
}

// Roots is a repository's statement of what a repository-root-relative path
// looks like: its top-level entries, plus where the application lives. It is
// the port of models.ScopeRoots — the board enforces the same rule from the
// copy Marshal publishes onto the project, and Marshal checks it locally so a
// bad path is caught before it is written rather than after.
//
// Empty Roots means "not declared" and checks nothing.
type Roots struct {
	Roots   []string
	AppRoot string
	// AppEntries are the app root's own top-level entries. A path starting
	// with one of these, when the repository root has no such entry, is the
	// app-relative spelling — the one ambiguity worth refusing.
	AppEntries []string
}

// ParseRoots reads the comma-separated forms stored on a project.
func ParseRoots(roots, appRoot, appEntries string) Roots {
	return Roots{
		Roots:      splitList(roots),
		AppRoot:    strings.Trim(strings.TrimSpace(appRoot), "/"),
		AppEntries: splitList(appEntries),
	}
}

func splitList(raw string) []string {
	var out []string
	for _, r := range strings.Split(raw, ",") {
		if r = strings.Trim(strings.TrimSpace(r), "/"); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// String renders the comma-separated form the board stores.
func (r Roots) String() string { return strings.Join(r.Roots, ",") }

// AppEntriesString renders the app root's entries the same way.
func (r Roots) AppEntriesString() string { return strings.Join(r.AppEntries, ",") }

// Declared reports whether there is enough here to check anything. Without the
// app root's entries there is no ambiguity to detect.
func (r Roots) Declared() bool { return len(r.Roots) > 0 && len(r.AppEntries) > 0 }

// Check rejects a canonical pattern whose first segment is not a top-level
// entry of the repository — that is, one written against the wrong base. The
// pattern must already have been through Canonical: this decides which base it
// is relative to, not how it is spelled.
//
// A first segment carrying a wildcard is accepted whatever it is: "**/logs"
// and "*.md" are anchored nowhere by construction, so there is no base to be
// wrong about.
func (r Roots) Check(pattern string) error {
	if !r.Declared() {
		return nil
	}
	_, rest := SplitRepo(pattern)
	first, _, _ := strings.Cut(rest, "/")
	if first == "" || segmentHasWildcard(first) {
		return nil
	}
	for _, root := range r.Roots {
		if first == root {
			return nil
		}
	}
	// Unknown to the repository root AND to the app root: a directory that
	// does not exist yet, which a task is entitled to claim. Only the
	// app-relative spelling is refused.
	var ambiguous bool
	for _, entry := range r.AppEntries {
		if first == entry {
			ambiguous = true
			break
		}
	}
	if !ambiguous {
		return nil
	}
	if s := r.Suggest(pattern); s != "" {
		return fmt.Errorf("scope path %q is not canonical: did you mean %q? paths are relative to the repository root (%s)",
			pattern, s, strings.Join(r.Roots, ", "))
	}
	return fmt.Errorf("scope path %q is not canonical: paths are relative to the repository root (%s)",
		pattern, strings.Join(r.Roots, ", "))
}

// Suggest offers the app-root-prefixed spelling when the project has an app
// root that is itself a valid root — the overwhelmingly common way to get this
// wrong is to write a path as the application sees it. It is a question, never
// a rewrite.
func (r Roots) Suggest(pattern string) string {
	if r.AppRoot == "" {
		return ""
	}
	var appRootIsARoot bool
	for _, root := range r.Roots {
		if root == r.AppRoot {
			appRootIsARoot = true
			break
		}
	}
	if !appRootIsARoot {
		return ""
	}
	repo, rest := SplitRepo(pattern)
	candidate := r.AppRoot + "/" + rest
	if repo != "" {
		candidate = repo + ":" + candidate
	}
	return candidate
}

// SplitRepo separates an optional `repo:` namespace from a pattern.
// Projects spanning several repositories prefix their paths so `src/index.ts`
// in one repo never collides with the same path in another; patterns without
// a prefix all live in the project's default repository. Only a prefix that
// looks like a name counts — a colon inside a path segment stays a path.
func SplitRepo(p string) (repo, rest string) {
	i := strings.Index(p, ":")
	if i <= 0 || strings.ContainsAny(p[:i], "/\\*?[") {
		return "", p
	}
	return p[:i], p[i+1:]
}

func validRepoName(repo string) bool {
	for _, r := range repo {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func segmentHasWildcard(seg string) bool {
	return strings.ContainsAny(seg, "*?[")
}

// literalPrefix returns the leading segments up to the first one containing a
// wildcard. Two patterns whose literal prefixes diverge can never match the
// same path.
func literalPrefix(segs []string) []string {
	for i, seg := range segs {
		if segmentHasWildcard(seg) {
			return segs[:i]
		}
	}
	return segs
}

func isPrefixOf(short, long []string) bool {
	if len(short) > len(long) {
		return false
	}
	for i := range short {
		if short[i] != long[i] {
			return false
		}
	}
	return true
}

// matchGlob reports whether a concrete path matches a pattern, with `**`
// spanning zero or more segments and every other segment going through
// path.Match.
func matchGlob(pattern, concrete []string) bool {
	if len(pattern) == 0 {
		return len(concrete) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(concrete); i++ {
			if matchGlob(pattern[1:], concrete[i:]) {
				return true
			}
		}
		return false
	}
	if len(concrete) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], concrete[0])
	if err != nil || !ok {
		return false
	}
	return matchGlob(pattern[1:], concrete[1:])
}

func hasWildcard(segs []string) bool {
	for _, s := range segs {
		if segmentHasWildcard(s) {
			return true
		}
	}
	return false
}

// coversSubtree is true for patterns that claim everything under their
// literal prefix: `dir/**`, `dir/**/*`, `**`.
func coversSubtree(segs []string) bool {
	lit := literalPrefix(segs)
	rest := segs[len(lit):]
	if len(rest) == 0 {
		return false
	}
	if rest[0] != "**" {
		return false
	}
	for _, s := range rest[1:] {
		if s != "*" && s != "**" {
			return false
		}
	}
	return true
}

// Covers reports whether a concrete, normalised file path falls under a scope
// pattern: the pattern names the file, names a directory above it, or is a
// glob it matches. Patterns and files in different repositories (repo:
// prefix) never cover each other.
func Covers(pattern, file string) bool {
	repoP, pattern := SplitRepo(pattern)
	repoF, file := SplitRepo(file)
	if repoP != repoF {
		return false
	}
	ps, fs := strings.Split(pattern, "/"), strings.Split(file, "/")
	if !hasWildcard(ps) {
		return isPrefixOf(ps, fs)
	}
	return matchGlob(ps, fs)
}

// MatchAny reports whether at least one pattern covers the file.
func MatchAny(patterns []string, file string) bool {
	for _, p := range patterns {
		if Covers(p, file) {
			return true
		}
	}
	return false
}

// Overlap reports whether two normalised scope patterns can name the same
// file. It is exact for the common shapes — concrete paths, a directory
// subtree, single-segment wildcards — and conservative for the rest: two
// wildcard patterns with a shared literal prefix and the same depth are
// treated as overlapping, so an exotic pair may be refused that a full
// intersection test would allow, but a real collision is never missed.
func Overlap(a, b string) bool {
	if a == b {
		return true
	}
	repoA, a := SplitRepo(a)
	repoB, b := SplitRepo(b)
	if repoA != repoB {
		return false
	}
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	la, lb := literalPrefix(as), literalPrefix(bs)
	if !isPrefixOf(la, lb) && !isPrefixOf(lb, la) {
		return false
	}

	aWild, bWild := hasWildcard(as), hasWildcard(bs)
	switch {
	case !aWild && !bWild:
		// Two concrete paths overlap only when equal (handled above) or when
		// one is a directory containing the other.
		return isPrefixOf(as, bs) || isPrefixOf(bs, as)
	case !aWild:
		return matchGlob(bs, as) || (isPrefixOf(as, lb))
	case !bWild:
		return matchGlob(as, bs) || (isPrefixOf(bs, la))
	}

	// Both have wildcards and share a literal prefix.
	if coversSubtree(as) || coversSubtree(bs) {
		return true
	}
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] == bs[i] || segmentHasWildcard(as[i]) || segmentHasWildcard(bs[i]) {
			continue
		}
		return false
	}
	return true
}
