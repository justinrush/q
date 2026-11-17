package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/justinrush/q/internal/paths"
	"github.com/spf13/cobra"
)

// saveFeatureFlags edits only the feature flag section of the active config.
func saveFeatureFlags(ctx context.Context, memory, nested bool) error {
	path := configPath()
	if path == "" {
		return fmt.Errorf("no configuration path is available")
	}
	if err := writeFeatureFlags(path, memory, nested); err != nil {
		return err
	}
	cfg.Features.Memory, cfg.Features.NestedMissions = memory, nested
	command := &cobra.Command{}
	command.SetContext(ctx)
	if err := restartDaemon(command); err != nil {
		return fmt.Errorf("flags were saved, but daemon restart failed: %w", err)
	}
	return nil
}

func writeFeatureFlags(path string, memory, nested bool) error {
	root := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) != 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return err
		}
	}
	flags, err := json.Marshal(featureFlagsConfig{Memory: memory, NestedMissions: nested})
	if err != nil {
		return err
	}
	root["featureFlags"] = flags
	delete(root, "memory")
	data, err = json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), paths.DirMode); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".q-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(paths.FileMode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return nil
}
