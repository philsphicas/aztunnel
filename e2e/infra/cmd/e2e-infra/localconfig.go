package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LocalConfig is the on-disk shape of e2e/.local.json. Mirrors the
// struct in e2e/testmain_helpers.go; duplicated here so the two
// modules stay decoupled. Field tags and ordering MUST match.
type LocalConfig struct {
	Subscription  string `json:"subscription"`
	Tenant        string `json:"tenant"`
	UserObjectID  string `json:"userObjectId"`
	ResourceGroup string `json:"resourceGroup"`
	RelayName     string `json:"relayName"`
	SASOnly       bool   `json:"sasOnly"`
	CreatedAt     string `json:"createdAt"`
}

// localConfigFilename is the basename written under e2e/.
const localConfigFilename = ".local.json"

// localConfigPath returns the absolute filesystem path of
// e2e/.local.json relative to the repository root. The CLI runs under
// e2e/infra (its module root) so the path is "../" + ".local.json".
//
// All subcommands resolve the path the same way; this is the single
// source of truth.
func localConfigPath() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(wd, "..", localConfigFilename)
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	// Defence-in-depth: the path's parent directory must be named
	// "e2e" — fail loudly if someone invokes us from an unexpected cwd
	// rather than silently writing the file in the wrong place.
	parent := filepath.Base(filepath.Dir(abs))
	if parent != "e2e" {
		return "", fmt.Errorf("expected to be invoked from e2e/infra (so cwd/../%s lives in e2e/), got parent=%q (cwd=%s)",
			localConfigFilename, parent, wd)
	}
	return abs, nil
}

// writeLocalConfig serialises cfg to e2e/.local.json with mode 0600.
// Overwrites any existing file.
func writeLocalConfig(cfg LocalConfig) (string, error) {
	path, err := localConfigPath()
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	// Trailing newline so editors don't fight the file on save.
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// readLocalConfig parses e2e/.local.json. Wraps os.ErrNotExist when
// the file is missing so callers can distinguish "no config yet" from
// "config is corrupt".
func readLocalConfig() (*LocalConfig, string, error) {
	path, err := localConfigPath()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a fixed basename + cwd.
	if err != nil {
		return nil, path, err
	}
	var cfg LocalConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, path, nil
}

// nowRFC3339 returns the current UTC instant as an RFC 3339 string.
// Indirected so tests can pin time.
var nowRFC3339 = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// printLocalConfigWritten prints the standard "config persisted" line
// shown after setup or attach. The path is rendered relative to the
// repo root for readability.
func printLocalConfigWritten(path string) {
	rel, err := filepath.Rel(filepath.Dir(filepath.Dir(path)), path)
	if err != nil {
		rel = path
	}
	fmt.Fprintf(os.Stderr, "==> wrote %s\n", rel)
}

// printAuthorizationFallbackNote writes the SAS-only fallback
// paragraph to stderr with an explicit grant command scoped to the
// requester's RG/namespace.
func printAuthorizationFallbackNote(sub, rg, relay string) {
	fmt.Fprintf(os.Stderr, "==> note: your Azure account does not have Microsoft.Authorization/roleAssignments/write\n"+
		"    on subscription %s, so I cannot grant you the Azure Relay data-plane role\n"+
		"    needed for Entra-authenticated tests. SAS-only mode will be used.\n"+
		"\n"+
		"    To enable Entra tests later, ask a subscription Owner (or anyone with User\n"+
		"    Access Administrator on the namespace) to run:\n"+
		"\n"+
		"        make e2e-grant RESOURCE_GROUP=%s RELAY_NAME=%s ASSIGNEE=<your-upn>\n"+
		"\n"+
		"    Then refresh your local config without retrying the role assignment:\n"+
		"\n"+
		"        make e2e-attach RESOURCE_GROUP=%s RELAY_NAME=%s\n",
		sub, rg, relay, rg, relay)
}
