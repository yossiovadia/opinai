// Package config provides shared configuration helpers used across packages.
package config

import (
	"encoding/json"
	"os"
	"strings"
)

// LoadRepoProfile loads a repo's JSON profile from the REPO_PROFILE_<repo> env var.
func LoadRepoProfile(repo string) map[string]any {
	r := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	key := "REPO_PROFILE_" + r.Replace(repo)
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return nil
	}
	return profile
}

// EnvOr returns the env var value or the fallback.
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
