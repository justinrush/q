// Package paths resolves the on-disk locations q uses.
//
// Callers construct a [Dirs] once, at process start, and pass it down. Leaf
// packages never call [Resolve] themselves, so tests can point everything at a
// t.TempDir().
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// File and directory modes. Everything q writes is user-only: state
// carries mission prompts, and daemon.json carries the API token.
const (
	// FileMode is the mode for every file q creates.
	FileMode os.FileMode = 0o600
	// ExecMode is the mode for generated scripts (launch.sh).
	ExecMode os.FileMode = 0o700
	// DirMode is the mode for every directory q creates.
	DirMode os.FileMode = 0o700
)

// appName is the per-user subdirectory used under each XDG root.
const appName = "q"

// Dirs holds the three XDG roots q writes beneath.
//
// Data holds durable user content (state, mission worktrees). State holds runtime
// ephemera (the daemon handle, the hook spool, logs). Config holds optional
// user overrides and is never written by q.
type Dirs struct {
	Data   string
	State  string
	Config string
}

// Overrides replaces the resolved locations with explicit ones. An empty field
// keeps the default for that directory.
//
// This is how ~/.q-config.json moves state onto another volume; the values reach
// here already expanded, because this package deliberately knows nothing about
// where they were configured.
type Overrides struct {
	Data  string
	State string
}

// Resolve determines the q directories, honoring the overrides first and then
// XDG_DATA_HOME, XDG_STATE_HOME, and XDG_CONFIG_HOME.
func Resolve(o Overrides) (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, fmt.Errorf("resolving home directory: %w", err)
	}

	dirs := Dirs{
		Data:   xdgRoot("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
		State:  xdgRoot("XDG_STATE_HOME", filepath.Join(home, ".local", "state")),
		Config: xdgRoot("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
	}

	if filepath.IsAbs(o.Data) {
		dirs.Data = o.Data
	}

	if filepath.IsAbs(o.State) {
		dirs.State = o.State
	}

	return dirs, nil
}

// xdgRoot returns the app subdirectory under $env, falling back to fallback
// when the variable is unset or not absolute (per the XDG spec, relative
// values must be ignored).
func xdgRoot(env, fallback string) string {
	root := os.Getenv(env)
	if !filepath.IsAbs(root) {
		root = fallback
	}

	return filepath.Join(root, appName)
}

// StateFile is the JSON snapshot of all operations and missions.
func (d Dirs) StateFile() string { return filepath.Join(d.Data, "state.json") }

// BackupFile is the previous good snapshot, used to recover a corrupt state file.
func (d Dirs) BackupFile() string { return filepath.Join(d.Data, "state.json.bak") }

// EventsFile is the append-only log of accepted mutations, for audit and replay.
func (d Dirs) EventsFile() string { return filepath.Join(d.Data, "events.ndjson") }

// MissionsDir holds one directory per mission, each containing that mission's worktrees.
func (d Dirs) MissionsDir() string { return filepath.Join(d.Data, "missions") }

// MissionDir is the working directory handed to a mission's agent. Its children are
// one git worktree per repo, plus the generated .q artifacts.
func (d Dirs) MissionDir(slug string) string { return filepath.Join(d.MissionsDir(), slug) }

// DaemonFile records the running daemon's pid, address, and API token. It is
// mode 0600 and is the only place the token is ever written.
func (d Dirs) DaemonFile() string { return filepath.Join(d.State, "daemon.json") }

// DaemonLockFile is flocked for the daemon's lifetime to enforce a single
// instance.
//
// It is deliberately separate from DaemonFile: that file is replaced by an
// atomic rename, which would swap the inode out from under any lock held on it.
func (d Dirs) DaemonLockFile() string { return filepath.Join(d.State, "daemon.lock") }

// SpoolDir holds hook events captured while the daemon was unreachable.
func (d Dirs) SpoolDir() string { return filepath.Join(d.State, "spool") }

// OrphansFile records hook events that resolved to no known mission.
func (d Dirs) OrphansFile() string { return filepath.Join(d.State, "orphans.ndjson") }

// LogsDir holds the daemon, TUI, and hook logs.
func (d Dirs) LogsDir() string { return filepath.Join(d.State, "logs") }

// LogFile returns the path to a named log, e.g. LogFile("daemon").
func (d Dirs) LogFile(name string) string {
	return filepath.Join(d.LogsDir(), name+".log")
}

// ConfigFile is the XDG-style location for q's config, checked after
// ~/.q-config.json. q reads it but never writes it.
func (d Dirs) ConfigFile() string { return filepath.Join(d.Config, "config.json") }

// Ensure creates every directory q writes to. It does not create the
// config directory, which q only ever reads.
func (d Dirs) Ensure() error {
	for _, dir := range []string{d.Data, d.MissionsDir(), d.State, d.SpoolDir(), d.LogsDir()} {
		if err := os.MkdirAll(dir, DirMode); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	return nil
}
