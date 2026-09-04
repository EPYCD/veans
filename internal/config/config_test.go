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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuggestedBotUsername(t *testing.T) {
	cases := map[string]string{
		"/home/user/myrepo": "bot-myrepo",
		"/tmp/My Project":   "bot-my-project",
		"/x/Hello_World":    "bot-hello-world",
		"/x/CRAZY---Name!!": "bot-crazy-name",
		"/x/.dotted":        "bot-dotted",
	}
	for in, want := range cases {
		if got := SuggestedBotUsername(in); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}

func TestFormatTaskID(t *testing.T) {
	withIdent := &Config{ProjectIdentifier: "PROJ"}
	if got := withIdent.FormatTaskID(7); got != "PROJ-7" {
		t.Errorf("got %q want PROJ-7", got)
	}
	noIdent := &Config{}
	if got := noIdent.FormatTaskID(7); got != "#7" {
		t.Errorf("got %q want #7", got)
	}
}

func TestFindAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	deeper := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(deeper, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Server:            "https://example.com",
		ProjectID:         42,
		ProjectIdentifier: "PROJ",
		ViewID:            7,
		Buckets:           Buckets{Todo: 1, InProgress: 2, InReview: 3, Done: 4, Scrapped: 5},
		Bot:               Bot{Username: "bot-test", UserID: 99},
	}
	if err := cfg.SaveAs(filepath.Join(dir, Filename)); err != nil {
		t.Fatal(err)
	}

	// Find from the deeper directory should walk up.
	found, err := Find(deeper)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !strings.HasSuffix(found, Filename) {
		t.Fatalf("found path %q does not end in %s", found, Filename)
	}
	loaded, err := Load(found)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ProjectID != 42 || loaded.Bot.Username != "bot-test" {
		t.Fatalf("unexpected reload shape: %+v", loaded)
	}
}

func TestFindMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := Find(dir); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// writeConfigPair writes a .veans.yml, plus a .veans.local.yml when local is
// non-empty, and returns the path to the committed one.
func writeConfigPair(t *testing.T, local string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, Filename)
	committed := "" +
		"server: https://board.example.com\n" +
		"project_id: 3\n" +
		"view_id: 12\n" +
		"buckets:\n  todo: 7\n  in_progress: 8\n  in_review: 10\n  done: 9\n  scrapped: 11\n" +
		"bot:\n  username: bot-capyard\n  user_id: 3\n"
	if err := os.WriteFile(path, []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	if local != "" {
		if err := os.WriteFile(filepath.Join(dir, LocalFilename), []byte(local), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Each person acts as their own bot, so claims and assignees name them,
// while the board coordinates stay shared.
func TestLoadLocalOverridesBotOnly(t *testing.T) {
	path := writeConfigPair(t, "bot:\n  username: bot-alice\n  user_id: 42\n")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bot.Username != "bot-alice" || c.Bot.UserID != 42 {
		t.Errorf("bot = %+v, want bot-alice/42", c.Bot)
	}
	if c.ProjectID != 3 || c.ViewID != 12 || c.Buckets.Todo != 7 {
		t.Errorf("local override leaked past bot: project=%d view=%d todo=%d",
			c.ProjectID, c.ViewID, c.Buckets.Todo)
	}
	if c.LocalPath() == "" {
		t.Error("LocalPath is empty after an override was applied")
	}
}

func TestLoadWithoutLocalKeepsCommittedBot(t *testing.T) {
	c, err := Load(writeConfigPair(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.Bot.Username != "bot-capyard" || c.Bot.UserID != 3 {
		t.Errorf("bot = %+v, want the committed bot-capyard/3", c.Bot)
	}
	if c.LocalPath() != "" {
		t.Errorf("LocalPath = %q, want empty", c.LocalPath())
	}
}

// Half an override would authenticate as one bot and filter tasks for
// another, which fails as a silent empty ready queue rather than an error.
func TestLoadLocalRejectsPartialBot(t *testing.T) {
	for name, local := range map[string]string{
		"username only": "bot:\n  username: bot-alice\n",
		"user_id only":  "bot:\n  user_id: 42\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfigPair(t, local))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), "both username and user_id") {
				t.Errorf("error does not explain what is missing: %v", err)
			}
		})
	}
}
