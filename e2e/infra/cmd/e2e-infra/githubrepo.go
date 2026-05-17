package main

import (
	"errors"
	"os"
	"strings"
)

// resolveGitHubRepo returns owner/repo, falling back to parsing the local
// git remote when the explicit value is empty. The fallback reads
// .git/config directly rather than shelling out to `git`.
func resolveGitHubRepo(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for _, path := range []string{".git/config", "../.git/config", "../../.git/config", "../../../.git/config"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if repo := parseGitConfigRepo(string(data)); repo != "" {
			return repo, nil
		}
	}
	return "", errors.New("--github-repo (or GITHUB_REPO env) is required when not running from a git checkout")
}

// parseGitConfigRepo extracts owner/repo from the first GitHub-style URL
// in a git config file. Supports https://github.com/owner/repo(.git) and
// git@github.com:owner/repo(.git). Returns "" if none found.
func parseGitConfigRepo(cfg string) string {
	for _, line := range strings.Split(cfg, "\n") {
		line = strings.TrimSpace(line)
		// Match only the `url = …` / `url=…` key, not adjacent keys such
		// as `urlInsteadOf` or `pushurl`.
		if !strings.HasPrefix(line, "url ") && !strings.HasPrefix(line, "url=") {
			continue
		}
		_, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.TrimSuffix(val, ".git")
		switch {
		case strings.HasPrefix(val, "https://github.com/"):
			return strings.TrimPrefix(val, "https://github.com/")
		case strings.HasPrefix(val, "git@github.com:"):
			return strings.TrimPrefix(val, "git@github.com:")
		}
	}
	return ""
}
