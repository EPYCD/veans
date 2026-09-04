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

// Package config reads and writes the per-repo .veans.yml file. The schema
// pins the project, view, canonical buckets, and bot identity so subsequent
// veans calls have everything they need without round-tripping to the server.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"code.vikunja.io/veans/internal/output"
)

// Filename is the canonical config name. Walked upward from cwd by Find.
const Filename = ".veans.yml"

// LocalFilename is the per-developer override read from beside Filename.
// It is never committed: add it to .gitignore.
const LocalFilename = ".veans.local.yml"

// Config is the on-disk shape of .veans.yml.
type Config struct {
	Server            string  `yaml:"server"`
	ProjectID         int64   `yaml:"project_id"`
	ProjectIdentifier string  `yaml:"project_identifier,omitempty"`
	ViewID            int64   `yaml:"view_id"`
	Buckets           Buckets `yaml:"buckets"`
	Bot               Bot     `yaml:"bot"`

	// Repository namespaces this checkout's scope paths (`repo:path`) so a
	// project spanning several repositories never confuses two files with
	// the same relative path. Leave empty for single-repository projects.
	Repository string `yaml:"repository,omitempty"`

	// HTTPTimeout overrides the default 30s HTTP client timeout when
	// non-zero. Accepts Go duration syntax in YAML ("60s", "5m", "1h30m").
	// Omitted from a freshly-written .veans.yml via omitempty so init flows
	// don't surface this knob unless the operator opts in.
	HTTPTimeout time.Duration `yaml:"http_timeout,omitempty"`

	path      string `yaml:"-"`
	localPath string `yaml:"-"`
}

// Local is the shape of .veans.local.yml. Only the bot identity is
// overridable: project, view and bucket IDs stay authoritative in the
// committed .veans.yml so everyone on a repo reads the same board, while
// each person can act as their own bot and get their own attribution.
type Local struct {
	Bot *Bot `yaml:"bot,omitempty"`
}

// Buckets maps the five canonical statuses to bucket IDs.
type Buckets struct {
	Todo       int64 `yaml:"todo"`
	InProgress int64 `yaml:"in_progress"`
	InReview   int64 `yaml:"in_review"`
	Done       int64 `yaml:"done"`
	Scrapped   int64 `yaml:"scrapped"`
}

// Bot identifies the Vikunja bot user veans operates as.
type Bot struct {
	Username string `yaml:"username"`
	UserID   int64  `yaml:"user_id"`
}

// Path returns the absolute path the config was loaded from (or written to).
func (c *Config) Path() string { return c.path }

// LocalPath returns the .veans.local.yml that overrode this config, or "".
func (c *Config) LocalPath() string { return c.localPath }

// FormatTaskID renders a numeric task index in the project's preferred form:
// PROJ-NN if the project has an identifier, #NN otherwise.
func (c *Config) FormatTaskID(index int64) string {
	if c.ProjectIdentifier != "" {
		return fmt.Sprintf("%s-%d", c.ProjectIdentifier, index)
	}
	return fmt.Sprintf("#%d", index)
}

// Find walks upward from cwd looking for .veans.yml. Returns ErrNotFound if
// none is reachable.
func Find(start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	dir := start
	for {
		candidate := filepath.Join(dir, Filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}

// ErrNotFound is returned by Find when no .veans.yml is reachable.
var ErrNotFound = errors.New(".veans.yml not found in any parent directory")

// Load reads .veans.yml from `path`. Use Find to locate it first.
func Load(path string) (*Config, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, output.Wrap(output.CodeNotConfigured, err, "no .veans.yml at %s", path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(buf, &c); err != nil {
		return nil, output.Wrap(output.CodeValidation, err, "parse %s: %v", path, err)
	}
	c.path = path
	if err := c.applyLocal(filepath.Join(filepath.Dir(path), LocalFilename)); err != nil {
		return nil, err
	}
	return &c, nil
}

// applyLocal overlays .veans.local.yml when it exists beside the config. A
// missing file is the normal case and not an error.
func (c *Config) applyLocal(path string) error {
	buf, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var l Local
	if err := yaml.Unmarshal(buf, &l); err != nil {
		return output.Wrap(output.CodeValidation, err, "parse %s: %v", path, err)
	}
	if l.Bot != nil {
		// Both halves are load-bearing: the token is looked up by username,
		// and "is this task mine?" is answered by user_id. Half an override
		// would authenticate as one bot and filter for another.
		if l.Bot.Username == "" || l.Bot.UserID == 0 {
			return output.New(output.CodeValidation,
				"%s: bot needs both username and user_id", path)
		}
		c.Bot = *l.Bot
	}
	c.localPath = path
	return nil
}

// SaveAs writes the config to `path` and remembers it as c.path.
func (c *Config) SaveAs(path string) error {
	c.path = path
	buf, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}

// RepoRoot returns the root of the git repo containing `start` (defaulting
// to cwd). When `start` is not in a git repo, RepoRoot returns the absolute
// `start` so callers can still derive a sensible bot username.
func RepoRoot(ctx context.Context, start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = start
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	abs, _ := filepath.Abs(start)
	return abs, nil
}

// SuggestedBotUsername proposes `bot-<reponame>` from a repo root path.
// Vikunja's username validator allows lowercase, digits, hyphens — we fold
// the basename to a safe shape.
func SuggestedBotUsername(root string) string {
	base := filepath.Base(root)
	var b strings.Builder
	b.WriteString("bot-")
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteRune('-')
		default:
			// drop other characters silently
		}
	}
	// Collapse runs of hyphens.
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.TrimRight(out, "-")
}
