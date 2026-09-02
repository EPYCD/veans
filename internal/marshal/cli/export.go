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

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"code.vikunja.io/veans/internal/client"
	veansconfig "code.vikunja.io/veans/internal/config"
	"code.vikunja.io/veans/internal/marshal/config"
	"code.vikunja.io/veans/internal/marshal/refs"
	"code.vikunja.io/veans/internal/output"
)

// This file is the seam `veans onboard` reaches through. Everything here
// delegates to the same code `marshal init` and `marshal setup` run, so the
// one-shot path can never drift from the commands it stands in for.

// InitOptions are the inputs that become a .marshal.yml. They mirror the
// flags of `marshal init`.
type InitOptions struct {
	AppRoot        string
	Codeowners     string
	PRD            string
	Spine          string
	Epics          string
	PublicURL      string
	DiscordWebhook string
	Ports          string
	Refs           []string
	Databases      []string
	Force          bool
}

// WriteConfig composes .marshal.yml in root and gitignores Marshal's state
// directory, returning the config and the path written. Shared by
// `marshal init` and `veans onboard`.
func WriteConfig(root string, o InitOptions) (*config.Config, string, error) {
	target := filepath.Join(root, config.Filename)
	if _, err := os.Stat(target); err == nil && !o.Force {
		return nil, "", output.New(output.CodeConflict, "%s exists — pass --force to overwrite", target)
	}

	cfg := &config.Config{AppRoot: o.AppRoot, Codeowners: o.Codeowners}
	for _, spec := range o.Refs {
		prefix, file, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, "", output.New(output.CodeValidation, "--ref %q: expected PREFIX=path/to/file.md", spec)
		}
		cfg.References = append(cfg.References, refs.Source{Prefix: prefix, File: file})
	}
	if len(cfg.References) == 0 {
		if o.PRD != "" {
			cfg.References = append(cfg.References, refs.Source{Prefix: "FR-", File: o.PRD}, refs.Source{Prefix: "NFR-", File: o.PRD})
		}
		if o.Spine != "" {
			cfg.References = append(cfg.References, refs.Source{Prefix: "AD-", File: o.Spine})
		}
		if o.Epics != "" {
			cfg.References = append(cfg.References, refs.Source{Prefix: "D-", File: o.Epics})
		}
	}
	cfg.Pool.Databases = o.Databases
	if o.Ports != "" {
		ports, err := parsePortRange(o.Ports)
		if err != nil {
			return nil, "", err
		}
		cfg.Pool.Ports = ports
	}
	cfg.Serve.PublicURL = o.PublicURL
	cfg.Discord.WebhookURL = o.DiscordWebhook

	if err := cfg.SaveAs(target); err != nil {
		return nil, "", err
	}
	if err := ensureIgnored(root, ".marshal/"); err != nil {
		return nil, "", err
	}
	return cfg, target, nil
}

// RunSetup provisions the Marshal and CI identities, sets the receipt bot
// and claim bucket, and registers the webhook — the body of
// `marshal setup`, callable in-process.
func RunSetup(ctx context.Context, w io.Writer, mcfg *config.Config, vcfg *veansconfig.Config, admin *client.Client, rotate, skipWebhook bool) error {
	return runSetup(ctx, w, mcfg, vcfg, admin, rotate, skipWebhook)
}
