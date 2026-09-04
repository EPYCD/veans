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

package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func read(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %v: %v", parts, err)
	}
	return string(b)
}

// The whole point of vendoring is that a coordinated repository never
// depends on another repository being readable. A cross-repo `uses:`
// sneaking back in would fail only at CI time, in someone else's repo.
func TestWriteVendorsTheActionAndNeverReferencesAnotherRepo(t *testing.T) {
	root := t.TempDir()
	res, err := Write(Options{Root: root, AppRoot: "web"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Written) == 0 {
		t.Fatal("nothing was written")
	}

	if _, err := os.Stat(filepath.Join(root, ActionDir, "action.yml")); err != nil {
		t.Fatalf("composite action not vendored: %v", err)
	}

	wf := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(wf)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("want 4 workflows, got %d", len(entries))
	}
	for _, e := range entries {
		body := read(t, wf, e.Name())
		if strings.Contains(body, "uses: EPYCD/") {
			t.Errorf("%s still references another repository", e.Name())
		}
		if strings.Contains(body, "@APP_") {
			t.Errorf("%s has an unsubstituted placeholder", e.Name())
		}
	}
}

// The scripts are invoked directly by action.yml, so losing the executable
// bit breaks CI with a permission error rather than anything descriptive.
func TestWriteMakesShellScriptsExecutable(t *testing.T) {
	root := t.TempDir()
	if _, err := Write(Options{Root: root}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, ActionDir, "common.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("common.sh is not executable: %v", info.Mode().Perm())
	}
}

// An app at the repository root must not produce "./src/..." — git
// diff --name-only never emits a leading "./", so the contract-change
// detection in the gates workflow would silently never fire.
func TestAppRootAtRepositoryRootProducesNoDotSlashPrefix(t *testing.T) {
	for _, appRoot := range []string{"", ".", "  "} {
		root := t.TempDir()
		if _, err := Write(Options{Root: root, AppRoot: appRoot}); err != nil {
			t.Fatalf("Write(%q): %v", appRoot, err)
		}
		body := read(t, root, ".github", "workflows", "gates.yml")
		if strings.Contains(body, "'^./src") || strings.Contains(body, "./package.json") {
			t.Errorf("AppRoot %q produced a ./-prefixed path", appRoot)
		}
		if got := workingDir(t, filepath.Join(root, ".github", "workflows", "gates.yml")); got != "." {
			t.Errorf("AppRoot %q: working-directory = %q, want \".\"", appRoot, got)
		}
	}
}

func TestAppRootSubstitution(t *testing.T) {
	root := t.TempDir()
	if _, err := Write(Options{Root: root, AppRoot: "captain-yard-web/"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	body := read(t, root, ".github", "workflows", "gates.yml")
	if got := workingDir(t, filepath.Join(root, ".github", "workflows", "gates.yml")); got != "captain-yard-web" {
		t.Errorf("working-directory = %q, want \"captain-yard-web\"", got)
	}
	if !strings.Contains(body, "captain-yard-web/package.json") {
		t.Error("app prefix missing from package_json_file")
	}
	if strings.Contains(body, "captain-yard-web//") {
		t.Error("trailing slash in AppRoot produced a doubled separator")
	}
}

// Re-running onboard on a live repository must not discard local edits.
func TestExistingFilesAreSkippedUnlessForced(t *testing.T) {
	root := t.TempDir()
	if _, err := Write(Options{Root: root}); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	mcp := filepath.Join(root, ".mcp.json")
	// 0o600: gosec's G306 caps written files at that, and the mode is
	// incidental here — the assertion is that Write skips an existing file.
	if err := os.WriteFile(mcp, []byte(`{"mine":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Write(Options{Root: root})
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if len(res.Written) != 0 {
		t.Errorf("second run overwrote %v", res.Written)
	}
	if got := read(t, mcp); got != `{"mine":true}` {
		t.Errorf("local edit was clobbered: %s", got)
	}

	if _, err := Write(Options{Root: root, Force: true}); err != nil {
		t.Fatalf("forced Write: %v", err)
	}
	if got := read(t, mcp); got == `{"mine":true}` {
		t.Error("--force did not replace the file")
	}
}

// Substitution happens by string replacement, so a placeholder in an
// unquoted scalar position could yield YAML that GitHub silently refuses to
// run. Parse what we actually write, for every shape of app root.
func TestRenderedWorkflowsAreValidYAML(t *testing.T) {
	for _, appRoot := range []string{"", ".", "web", "captain-yard-web/"} {
		root := t.TempDir()
		if _, err := Write(Options{Root: root, AppRoot: appRoot}); err != nil {
			t.Fatalf("Write(%q): %v", appRoot, err)
		}
		dir := filepath.Join(root, ".github", "workflows")
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read workflows: %v", err)
		}
		for _, e := range entries {
			var doc any
			if err := yaml.Unmarshal([]byte(read(t, dir, e.Name())), &doc); err != nil {
				t.Errorf("AppRoot %q: %s is not valid YAML: %v", appRoot, e.Name(), err)
			}
		}
		// The vendored action must parse too — CI reads it as YAML.
		var action any
		if err := yaml.Unmarshal([]byte(read(t, root, ActionDir, "action.yml")), &action); err != nil {
			t.Errorf("vendored action.yml is not valid YAML: %v", err)
		}
	}
}

// workingDir returns defaults.run.working-directory from a rendered
// workflow, so assertions survive changes in how the value is quoted.
func workingDir(t *testing.T, path string) string {
	t.Helper()
	var doc struct {
		Defaults struct {
			Run struct {
				WorkingDirectory string `yaml:"working-directory"`
			} `yaml:"run"`
		} `yaml:"defaults"`
	}
	if err := yaml.Unmarshal([]byte(read(t, path)), &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.Defaults.Run.WorkingDirectory
}
