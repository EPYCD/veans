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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"code.vikunja.io/veans/internal/output"
)

// gitRepo builds a real repository with an "origin" remote, because the whole
// question here is what git says the default branch is — a fake would only
// test the fake.
func gitRepo(t *testing.T, defaultBranch string, branches ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	root := t.TempDir()
	origin := root + "/origin"
	work := root + "/work"

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
		}
	}

	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	run(origin, "init", "--initial-branch="+defaultBranch)
	if err := os.WriteFile(origin+"/README", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(origin, "add", "README")
	run(origin, "commit", "-m", "first")
	for _, b := range branches {
		run(origin, "branch", b)
	}
	run(root, "clone", origin, "work")
	return work
}

// inDir runs f with the process working directory set to dir. claimBranch
// shells out to git in the current directory, which is what veans itself does.
func inDir(t *testing.T, dir string, f func()) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()
	f()
}

// TestClaimBranch_DefaultBranchIsNotAClaim is the regression test for 23
// CapYard tasks labelled veans:branch:main. Claiming before branching is the
// natural order — you often branch off the ticket — so the label recorded
// whatever HEAD happened to be, and on a third of the board it said "main":
// no information, and `veans list --branch` silently under-reporting.
func TestClaimBranch_DefaultBranchIsNotAClaim(t *testing.T) {
	repo := gitRepo(t, "main", "e5.3-scheduler")

	inDir(t, repo, func() {
		var warn bytes.Buffer
		got := claimBranch(context.Background(), &warn)
		if got != "" {
			t.Errorf("claiming from the default branch must record nothing, got %q", got)
		}
		if !strings.Contains(warn.String(), "default branch") {
			t.Errorf("and must say so: %q", warn.String())
		}
		if !strings.Contains(warn.String(), "--branch") {
			t.Errorf("the warning should say how to fix it: %q", warn.String())
		}
	})
}

func TestClaimBranch_WorkingBranchIsRecorded(t *testing.T) {
	repo := gitRepo(t, "main", "e5.3-scheduler")
	cmd := exec.CommandContext(t.Context(), "git", "checkout", "e5.3-scheduler")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v\n%s", err, out)
	}

	inDir(t, repo, func() {
		var warn bytes.Buffer
		if got := claimBranch(context.Background(), &warn); got != "e5.3-scheduler" {
			t.Errorf("claimBranch = %q, want e5.3-scheduler", got)
		}
		if warn.Len() != 0 {
			t.Errorf("a real branch warns about nothing: %q", warn.String())
		}
	})
}

// TestClaimBranch_DefaultIsWhateverGitSays: "main" is a convention, not a
// rule. A repository whose default is "trunk" must not have "trunk" recorded
// and must not be warned about a branch called "main".
func TestClaimBranch_DefaultIsWhateverGitSays(t *testing.T) {
	repo := gitRepo(t, "trunk", "main")

	inDir(t, repo, func() {
		var warn bytes.Buffer
		if got := claimBranch(context.Background(), &warn); got != "" {
			t.Errorf("on trunk, the default here, claimBranch = %q", got)
		}
	})

	cmd := exec.CommandContext(t.Context(), "git", "checkout", "main")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v\n%s", err, out)
	}
	inDir(t, repo, func() {
		var warn bytes.Buffer
		if got := claimBranch(context.Background(), &warn); got != "main" {
			t.Errorf("main is a working branch here; claimBranch = %q", got)
		}
	})
}

// TestClaimBranch_OutsideARepository: veans is run in all sorts of places.
// No git, no branch, no label, no warning.
func TestClaimBranch_OutsideARepository(t *testing.T) {
	inDir(t, t.TempDir(), func() {
		var warn bytes.Buffer
		if got := claimBranch(context.Background(), &warn); got != "" {
			t.Errorf("claimBranch = %q outside a repository", got)
		}
		if warn.Len() != 0 {
			t.Errorf("nothing to warn about: %q", warn.String())
		}
	})
}

// claimServer answers the atomic claim and the reads around it. refuse decides
// what POST /tasks/42/claim says: 0 for a claim that takes (or re-takes) the
// task, an HTTP status for a refusal.
func claimServer(t *testing.T, labels []string, refuse int) (*httptest.Server, *[]recordedCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []recordedCall
	)
	task := map[string]any{"id": 42, "index": 7, "title": "t", "assignees": []map[string]any{{"id": 5}}}
	ls := []map[string]any{}
	for i, l := range labels {
		ls = append(ls, map[string]any{"id": 100 + i, "title": l})
	}
	task["labels"] = ls
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, recordedCall{method: r.Method, path: r.URL.Path})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/tasks/42/claim":
			if refuse != 0 {
				w.WriteHeader(refuse)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "refused"})
				return
			}
			_ = json.NewEncoder(w).Encode(task)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tasks/42":
			_ = json.NewEncoder(w).Encode(task)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "total_pages": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99, "title": "veans:branch:feat"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/tasks/42/labels":
			http.Error(w, `{"message":"no"}`, http.StatusForbidden)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestRunClaim_RepeatIsANoOp: claiming a task you already hold succeeds, and
// costs exactly the atomic claim plus one read — no assignee write, no label
// write, nothing the server could answer "already there" to.
func TestRunClaim_RepeatIsANoOp(t *testing.T) {
	srv, calls := claimServer(t, []string{"veans:branch:feat"}, 0)
	rt := newTestRuntime(srv.URL)

	for range 2 {
		if _, err := runClaim(context.Background(), rt, 42, &claimOptions{ref: "#7", branch: "feat", warn: io.Discard}); err != nil {
			t.Fatalf("runClaim: %v", err)
		}
	}

	once := []recordedCall{
		{http.MethodPost, "/api/v2/tasks/42/claim"},
		{http.MethodGet, "/api/v2/tasks/42"},
	}
	if want := append(append([]recordedCall{}, once...), once...); !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls:\nwant: %#v\ngot:  %#v", want, *calls)
	}
}

// TestRunClaim_ConflictTouchesNothingElse: a refused claim is the end of it.
// No read, no label — the task belongs to somebody else.
func TestRunClaim_ConflictTouchesNothingElse(t *testing.T) {
	srv, calls := claimServer(t, nil, http.StatusConflict)
	rt := newTestRuntime(srv.URL)

	_, err := runClaim(context.Background(), rt, 42, &claimOptions{ref: "#7", branch: "feat", warn: io.Discard})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != output.CodeConflict {
		t.Fatalf("want CONFLICT, got %v", err)
	}
	if oe.Task != "" || len(oe.Completed) != 0 {
		t.Fatalf("a refused claim changed nothing and must not report progress: %+v", oe)
	}
	if want := []recordedCall{{http.MethodPost, "/api/v2/tasks/42/claim"}}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls:\nwant: %#v\ngot:  %#v", want, *calls)
	}
}

// TestRunClaim_LabelFailureReportsTheClaim: the claim took, the label did not.
// The error has to say the task is already held, or the caller retries a claim
// that is not the problem.
func TestRunClaim_LabelFailureReportsTheClaim(t *testing.T) {
	srv, _ := claimServer(t, nil, 0)
	rt := newTestRuntime(srv.URL)

	_, err := runClaim(context.Background(), rt, 42, &claimOptions{ref: "#7", branch: "feat", warn: io.Discard})
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want an output.Error, got %v", err)
	}
	if oe.Task != "#7" || !reflect.DeepEqual(oe.Completed, []string{"claim"}) || oe.Failed != "label veans:branch:feat" {
		t.Fatalf("partial progress not reported: task=%q completed=%v failed=%q", oe.Task, oe.Completed, oe.Failed)
	}
}
