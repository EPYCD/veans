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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"code.vikunja.io/veans/internal/auth"
	"code.vikunja.io/veans/internal/bootstrap"
	"code.vikunja.io/veans/internal/client"
	"code.vikunja.io/veans/internal/config"
	marshalcli "code.vikunja.io/veans/internal/marshal/cli"
	"code.vikunja.io/veans/internal/output"
	"code.vikunja.io/veans/internal/scaffold"
)

type onboardFlags struct {
	server      string
	token       string
	usePassword bool
	username    string
	password    string
	totp        string
	botUsername string
	projectID   int64
	viewID      int64
	appRoot     string
	publicURL   string
	discord     string
	codeowners  string
	prd         string
	spine       string
	epics       string
	skipWebhook bool
	force       bool
}

// onboardResult is what the command prints. It is deliberately a superset
// of what the individual steps report, so a single run leaves a record of
// everything that was provisioned.
type onboardResult struct {
	Server      string           `json:"server"`
	ProjectID   int64            `json:"project_id"`
	ViewID      int64            `json:"view_id"`
	Bot         string           `json:"bot"`
	VeansConfig string           `json:"veans_config"`
	MarshalCfg  string           `json:"marshal_config"`
	Scaffold    *scaffold.Result `json:"scaffold"`
	Next        []string         `json:"next"`
}

func newOnboardCmd() *cobra.Command {
	f := &onboardFlags{}
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Provision the board, the bots and every repository file in one run",
		Long: `Takes a repository with nothing wired up and leaves it fully coordinated.
Equivalent to running, in order:

  veans init      project (created if it does not exist), the five canonical
                  buckets, the agent bot, the project share, its token,
                  .veans.yml and .claude/settings.json
  marshal init    .marshal.yml, and .marshal/ added to .gitignore
  marshal setup   bot-marshal-<repo> and bot-ci-<repo>, the project's receipt
                  bot, the view's claim bucket, and the board webhook

and then writing the three things none of those produce: .mcp.json, the four
board workflows, and the composite action they call.

You authenticate once. The same token drives every step, so a project admin
runs this and nothing else is asked for.

The action is vendored into .github/actions/ rather than referenced across
repositories, because a cross-repo "uses:" only resolves when the action's
repository is public or private under the same owner — usually neither.

Existing files are never overwritten unless --force is given, so this is
safe to re-run on a half-configured repository.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			errOut := cmd.ErrOrStderr()

			root, err := config.RepoRoot(ctx, "")
			if err != nil {
				return output.Wrap(output.CodeUnknown, err, "detect repo root: %v", err)
			}

			// One authentication for the whole run. bootstrap.Init would
			// otherwise acquire its own token and keep it, leaving setup
			// with nothing to act as an admin with.
			if f.server == "" {
				return output.New(output.CodeValidation, "--server is required (e.g. --server https://board.example.com)")
			}
			canonical, _, err := client.DiscoverServer(ctx, f.server)
			if err != nil {
				return output.Wrap(output.CodeUnknown, err, "discover server: %v", err)
			}
			adminToken, err := auth.AcquireHumanToken(ctx, client.New(canonical, ""), auth.LoginOptions{
				Token:       f.token,
				UsePassword: f.usePassword,
				Username:    f.username,
				Password:    f.password,
				TOTP:        f.totp,
				Out:         errOut,
			}, auth.NewStdPrompter())
			if err != nil {
				return err
			}

			fmt.Fprintln(errOut, "== 1/4  board, bot and .veans.yml")
			initRes, err := bootstrap.Init(ctx, &bootstrap.Options{
				ConfigPath:         filepath.Join(root, config.Filename),
				RepoRoot:           root,
				Server:             canonical,
				HumanToken:         adminToken,
				BotUsername:        f.botUsername,
				ProjectID:          f.projectID,
				ViewID:             f.viewID,
				AutoApproveBuckets: true,
				InstallClaudeCode:  true,
				ClaudeCodeFlagSet:  true,
				Out:                errOut,
			})
			if err != nil {
				return err
			}

			fmt.Fprintln(errOut, "== 2/4  .marshal.yml")
			mcfg, mpath, err := marshalcli.WriteConfig(root, marshalcli.InitOptions{
				AppRoot:        f.appRoot,
				Codeowners:     f.codeowners,
				PRD:            f.prd,
				Spine:          f.spine,
				Epics:          f.epics,
				PublicURL:      f.publicURL,
				DiscordWebhook: f.discord,
				Force:          f.force,
			})
			if err != nil {
				return err
			}

			fmt.Fprintln(errOut, "== 3/4  service bots, receipt bot and webhook")
			vcfg, err := config.Load(initRes.Config.Path())
			if err != nil {
				return err
			}
			if err := marshalcli.RunSetup(ctx, errOut, mcfg, vcfg, client.New(canonical, adminToken), false, f.skipWebhook); err != nil {
				return err
			}

			fmt.Fprintln(errOut, "== 4/4  .mcp.json, workflows and the vendored action")
			sc, err := scaffold.Write(scaffold.Options{
				Root:    root,
				AppRoot: f.appRoot,
				Force:   f.force,
			})
			if err != nil {
				return err
			}

			res := &onboardResult{
				Server:      canonical,
				ProjectID:   vcfg.ProjectID,
				ViewID:      vcfg.ViewID,
				Bot:         vcfg.Bot.Username,
				VeansConfig: initRes.Config.Path(),
				MarshalCfg:  mpath,
				Scaffold:    sc,
				Next: []string{
					"add the ci_token printed above as the repository secret WORKMAN_TOKEN — the gates cannot close a task without it",
					"commit .veans.yml, .marshal.yml, .mcp.json, .github/workflows/ and .github/actions/",
					"export VEANS_TOKEN so the MCP server can authenticate, then start Claude in this repo",
				},
			}
			if len(sc.Skipped) > 0 {
				res.Next = append(res.Next, fmt.Sprintf("%d file(s) already existed and were left alone; re-run with --force to replace them", len(sc.Skipped)))
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(res)
		},
	}

	cmd.Flags().StringVar(&f.server, "server", os.Getenv("VEANS_SERVER"), "board URL, e.g. https://board.example.com")
	cmd.Flags().StringVar(&f.token, "token", "", "a project admin's JWT or API token (skips the interactive login)")
	cmd.Flags().BoolVar(&f.usePassword, "use-password", false, "use POST /login instead of the default OAuth flow")
	cmd.Flags().StringVar(&f.username, "username", "", "your board username (implies --use-password)")
	cmd.Flags().StringVar(&f.password, "password", "", "your board password (implies --use-password; prompted if empty)")
	cmd.Flags().StringVar(&f.totp, "totp", "", "TOTP code if your account requires 2FA")
	cmd.Flags().StringVar(&f.botUsername, "bot-username", "", "override the bot-<repo> default")
	cmd.Flags().Int64Var(&f.projectID, "project", 0, "use this project instead of picking or creating one")
	cmd.Flags().Int64Var(&f.viewID, "view", 0, "use this Kanban view instead of picking one")
	cmd.Flags().StringVar(&f.appRoot, "app-root", "", "sub-directory the board's paths are relative to (e.g. web); empty means the repository root")
	cmd.Flags().StringVar(&f.publicURL, "public-url", "", "how the board and CI reach Marshal; without it no webhook is registered")
	cmd.Flags().StringVar(&f.discord, "discord-webhook", "", "Discord channel webhook URL")
	cmd.Flags().StringVar(&f.codeowners, "codeowners", ".github/CODEOWNERS", "CODEOWNERS path")
	cmd.Flags().StringVar(&f.prd, "prd", "", "PRD file holding FR-/NFR- anchors")
	cmd.Flags().StringVar(&f.spine, "spine", "", "architecture file holding AD- anchors")
	cmd.Flags().StringVar(&f.epics, "epics", "", "epics file holding D- anchors")
	cmd.Flags().BoolVar(&f.skipWebhook, "skip-webhook", false, "do not register the board webhook")
	cmd.Flags().BoolVar(&f.force, "force", false, "overwrite files that already exist")
	return cmd
}
