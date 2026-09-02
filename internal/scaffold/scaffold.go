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

// Package scaffold writes the repository-side files that nothing else
// creates: the MCP server declaration an agent's Claude reads, the four
// board workflows, and the composite action those workflows call.
//
// The action is vendored into the target repository rather than referenced
// as `owner/repo/.github/actions/...@main`. A cross-repository `uses:` only
// resolves when the action's repository is public, or private within the
// same owner; a coordinated repo is frequently neither. Copying the six
// files in costs nothing and works everywhere.
//
// Substitution is literal (@APP_ROOT@, @APP_PREFIX@) rather than
// text/template, because the workflows are dense with GitHub Actions
// expressions — ${{ github.sha }} — that a Go template would try to parse.
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates
var files embed.FS

// ActionDir is where the composite action is vendored, relative to the
// repository root. The workflows' `uses:` lines point at this path.
const ActionDir = ".github/actions/workman-merge-hook"

// Options describe the repository a scaffold is written into.
type Options struct {
	// Root is the repository root — the directory holding .veans.yml.
	Root string
	// AppRoot mirrors .marshal.yml's app_root: the sub-directory the
	// board's paths are relative to. "" and "." both mean the root.
	AppRoot string
	// Force overwrites files that already exist. Without it an existing
	// file is left untouched and reported as skipped, so re-running
	// onboard on a live repository never clobbers local edits.
	Force bool
}

// Result records what was written and what was left alone.
type Result struct {
	Written []string `json:"written"`
	Skipped []string `json:"skipped"`
}

// appRoot returns the value for @APP_ROOT@ (a directory, "." at the root)
// and @APP_PREFIX@ (a path prefix, empty at the root so that "src/x" does
// not become "./src/x" — which would not match `git diff --name-only`).
func (o Options) appRoot() (root, prefix string) {
	a := strings.Trim(strings.TrimSpace(o.AppRoot), "/")
	if a == "" || a == "." {
		return ".", ""
	}
	return a, a + "/"
}

// Write lays down the MCP declaration, the vendored action and the four
// workflows. Directories are created as needed; existing files are skipped
// unless Force is set.
func Write(o Options) (*Result, error) {
	if o.Root == "" {
		return nil, fmt.Errorf("scaffold: no repository root given")
	}
	root, prefix := o.appRoot()
	rep := strings.NewReplacer("@APP_ROOT@", root, "@APP_PREFIX@", prefix)
	res := &Result{Written: []string{}, Skipped: []string{}}

	if err := o.copyTree(res, "templates/action", ActionDir, nil); err != nil {
		return nil, err
	}
	if err := o.copyTree(res, "templates/workflows", ".github/workflows", rep); err != nil {
		return nil, err
	}
	if err := o.copyFile(res, "templates/mcp.json.tmpl", ".mcp.json", nil); err != nil {
		return nil, err
	}
	return res, nil
}

// copyTree writes every file in an embedded directory into dst.
func (o Options) copyTree(res *Result, src, dst string, rep *strings.Replacer) error {
	entries, err := fs.ReadDir(files, src)
	if err != nil {
		return fmt.Errorf("scaffold: read %s: %w", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := o.copyFile(res, src+"/"+e.Name(), filepath.Join(dst, e.Name()), rep); err != nil {
			return err
		}
	}
	return nil
}

// copyFile writes one embedded file, substituting placeholders when rep is
// non-nil. Shell scripts are written executable; the action's action.yml
// invokes them directly.
func (o Options) copyFile(res *Result, src, rel string, rep *strings.Replacer) error {
	body, err := files.ReadFile(src)
	if err != nil {
		return fmt.Errorf("scaffold: read %s: %w", src, err)
	}
	if rep != nil {
		body = []byte(rep.Replace(string(body)))
	}
	target := filepath.Join(o.Root, filepath.FromSlash(rel))
	if _, err := os.Stat(target); err == nil && !o.Force {
		res.Skipped = append(res.Skipped, rel)
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scaffold: stat %s: %w", target, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("scaffold: create %s: %w", filepath.Dir(target), err)
	}
	mode := os.FileMode(0o644)
	if strings.HasSuffix(rel, ".sh") {
		mode = 0o755
	}
	if err := os.WriteFile(target, body, mode); err != nil {
		return fmt.Errorf("scaffold: write %s: %w", target, err)
	}
	res.Written = append(res.Written, rel)
	return nil
}
