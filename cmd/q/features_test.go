package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFeatureFlagsPreservesOtherConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q-config.json")
	if err := os.WriteFile(path, []byte(`{"repos":{"roots":["/dev"]},"memory":{"enabled":true,"captureOnFinish":false},"featureFlags":{"memory":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeFeatureFlags(path, featureFlagsConfig{NestedMissions: true, DebriefPanes: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var repos struct {
		Roots []string `json:"roots"`
	}
	if err := json.Unmarshal(root["repos"], &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos.Roots) != 1 || repos.Roots[0] != "/dev" || root["memory"] != nil {
		t.Fatalf("unrelated config changed or legacy setting kept: %s", data)
	}
	var flags featureFlagsConfig
	if err := json.Unmarshal(root["featureFlags"], &flags); err != nil {
		t.Fatal(err)
	}
	if flags.Memory || !flags.NestedMissions || !flags.DebriefPanes {
		t.Fatalf("flags: %+v", flags)
	}
}
