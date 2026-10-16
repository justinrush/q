package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// TrustProject writes only this mission's trust entry through Codex's config
// API. Remote TUI startup checks the server's base user config before applying
// a per-thread profile, so a trust entry in q.config.toml alone is insufficient.
// Codex owns parsing and preserving the rest of the user's configuration.
func (p *Proxy) TrustProject(ctx context.Context, directory string) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return fmt.Errorf("resolving mission directory: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("mission path is not a directory: %s", canonical)
	}
	params := struct {
		KeyPath       string `json:"keyPath"`
		Value         string `json:"value"`
		MergeStrategy string `json:"mergeStrategy"`
	}{fmt.Sprintf("projects.%q.trust_level", canonical), "trusted", "upsert"}
	return p.client.Request(ctx, "config/value/write", params, nil)
}
