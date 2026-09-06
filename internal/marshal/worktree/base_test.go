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
	"reflect"
	"strings"
	"testing"
)

// TestBuildCutsFromTheIntegrationBranch is the whole of enforcement point 1:
// a worktree with no start point branches from the invoking checkout's current
// HEAD, so a worker running this in a clone they have not pulled in a week
// starts a week behind — in the files they are about to edit. The command that
// exists to set a worker up correctly was seeding the staleness.
func TestBuildCutsFromTheIntegrationBranch(t *testing.T) {
	p, err := Build(BuildOptions{
		RepoRoot: "/src/capyard",
		Story:    "E5.3",
		Title:    "A scheduler",
		Base:     "origin/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		// Fetch first, or the start point is whatever this checkout last saw
		// of the integration branch, which is the same staleness by a
		// different route.
		"git fetch origin",
		"git worktree add ../capyard-e5.3 -b e5.3-scheduler origin/main",
		"cd ../capyard-e5.3",
	}
	if !reflect.DeepEqual(p.Commands, want) {
		t.Fatalf("commands =\n  %v\nwant\n  %v", p.Commands, want)
	}
	if p.Base != "origin/main" {
		t.Errorf("Base = %q", p.Base)
	}
}

func TestBuildWithoutABaseKeepsTheOldShape(t *testing.T) {
	// A repository with no remote still gets a worktree; it just cannot be
	// given a start point that does not exist.
	p, err := Build(BuildOptions{RepoRoot: "/src/capyard", Story: "E5.3", Title: "A scheduler"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"git worktree add ../capyard-e5.3 -b e5.3-scheduler",
		"cd ../capyard-e5.3",
	}
	if !reflect.DeepEqual(p.Commands, want) {
		t.Fatalf("commands = %v, want %v", p.Commands, want)
	}
}

func TestBuildBaseComesBeforeTheEnvFile(t *testing.T) {
	p, err := Build(BuildOptions{
		RepoRoot: "/src/capyard", Story: "E5.3", Title: "A scheduler",
		Base: "upstream/trunk", Port: 3100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Commands[0] != "git fetch upstream" {
		t.Errorf("the remote is taken from the base ref: %q", p.Commands[0])
	}
	if !strings.HasSuffix(p.Commands[1], " upstream/trunk") {
		t.Errorf("start point missing: %q", p.Commands[1])
	}
	if last := p.Commands[len(p.Commands)-1]; !strings.Contains(last, ".env.local") {
		t.Errorf("the env line must still come last: %q", last)
	}
}

func TestRemoteOf(t *testing.T) {
	for base, want := range map[string]string{
		"origin/main":       "origin",
		"upstream/trunk":    "upstream",
		"origin/release/v2": "origin",
		// A bare branch names no remote, so the default one is fetched.
		"main": "origin",
		"":     "origin",
	} {
		if got := RemoteOf(base); got != want {
			t.Errorf("RemoteOf(%q) = %q, want %q", base, got, want)
		}
	}
}
