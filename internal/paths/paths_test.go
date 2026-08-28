package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHonorsXDGVars(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")

	dirs, err := Resolve(Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for _, tc := range []struct{ name, got, want string }{
		{"data", dirs.Data, "/xdg/data/q"},
		{"state", dirs.State, "/xdg/state/q"},
		{"config", dirs.Config, "/xdg/config/q"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// The XDG spec requires relative values to be ignored rather than resolved
// against the cwd, which would scatter state wherever q happened to be run.
func TestResolveIgnoresRelativeXDGVars(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "relative/path")
	t.Setenv("XDG_STATE_HOME", "")

	dirs, err := Resolve(Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	wantData := filepath.Join(home, ".local", "share", "q")
	if dirs.Data != wantData {
		t.Errorf("Data = %q, want %q", dirs.Data, wantData)
	}

	wantState := filepath.Join(home, ".local", "state", "q")
	if dirs.State != wantState {
		t.Errorf("State = %q, want %q", dirs.State, wantState)
	}
}

func TestDirsLayout(t *testing.T) {
	dirs := Dirs{Data: "/d", State: "/s", Config: "/c"}

	for _, tc := range []struct{ name, got, want string }{
		{"StateFile", dirs.StateFile(), "/d/state.json"},
		{"BackupFile", dirs.BackupFile(), "/d/state.json.bak"},
		{"EventsFile", dirs.EventsFile(), "/d/events.ndjson"},
		{"MissionsDir", dirs.MissionsDir(), "/d/missions"},
		{"MissionDir", dirs.MissionDir("operation--mission"), "/d/missions/operation--mission"},
		{"DaemonFile", dirs.DaemonFile(), "/s/daemon.json"},
		{"SpoolDir", dirs.SpoolDir(), "/s/spool"},
		{"OrphansFile", dirs.OrphansFile(), "/s/orphans.ndjson"},
		{"LogsDir", dirs.LogsDir(), "/s/logs"},
		{"LogFile", dirs.LogFile("daemon"), "/s/logs/daemon.log"},
		{"ConfigFile", dirs.ConfigFile(), "/c/config.json"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// State holds mission prompts and the daemon API token, so every directory q
// creates must be user-only.
func TestEnsureCreatesUserOnlyDirs(t *testing.T) {
	root := t.TempDir()
	dirs := Dirs{
		Data:   filepath.Join(root, "data"),
		State:  filepath.Join(root, "state"),
		Config: filepath.Join(root, "config"),
	}

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, dir := range []string{dirs.Data, dirs.MissionsDir(), dirs.State, dirs.SpoolDir(), dirs.LogsDir()} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)

			continue
		}

		if perm := fi.Mode().Perm(); perm != DirMode {
			t.Errorf("%s mode = %v, want %v", dir, perm, DirMode)
		}
	}

	// q only reads the config dir, so it must not be created.
	if _, err := os.Stat(dirs.Config); !os.IsNotExist(err) {
		t.Errorf("config dir should not be created, stat err = %v", err)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	dirs := Dirs{Data: filepath.Join(t.TempDir(), "d"), State: filepath.Join(t.TempDir(), "s")}

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}
