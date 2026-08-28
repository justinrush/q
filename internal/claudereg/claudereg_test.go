package claudereg

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The registry directory holds messaging credentials alongside the status files.
// This is the guard that q never opens them: a decoy key file is planted with
// a mode that would make reading it detectable, and the scan must ignore it.
func TestScanNeverReadsKeyFiles(t *testing.T) {
	dir := t.TempDir()

	writeSession(t, dir, os.Getpid(), "live-session", "busy")

	// A credential file, named exactly as claude names them.
	decoy := filepath.Join(dir, fmt.Sprintf("%d.abc123def456.key", os.Getpid()))
	if err := os.WriteFile(decoy, []byte("SECRET-DO-NOT-READ"), 0o600); err != nil {
		t.Fatalf("writing decoy: %v", err)
	}

	sessions, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("len = %d, want only the json entry", len(sessions))
	}

	for _, session := range sessions {
		if session.SessionID == "" {
			t.Error("a key file appears to have been parsed as a session")
		}
	}
}

// read is the only function that opens a file, so it must refuse anything that is
// not plainly a JSON entry even if the glob above were ever widened.
func TestReadRefusesNonJSONPaths(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "1234.abc.key")
	if err := os.WriteFile(path, []byte(`{"pid":1,"sessionId":"x"}`), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	if _, ok := read(path); ok {
		t.Error("read accepted a .key path")
	}
}

// writeSession creates a registry entry.
func writeSession(t *testing.T, dir string, pid int, sessionID, status string) {
	t.Helper()

	body := fmt.Sprintf(
		`{"pid":%d,"sessionId":%q,"cwd":"/tmp","status":%q,"waitingFor":"input needed",`+
			`"tmux":"jarush:@4.%%13","updatedAt":1786975627255,"kind":"interactive"}`,
		pid, sessionID, status)

	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing session: %v", err)
	}
}

// Entries are keyed by pid and outlive the process, so a dead one must be ignored
// rather than believed.
func TestScanSkipsDeadProcesses(t *testing.T) {
	dir := t.TempDir()

	writeSession(t, dir, os.Getpid(), "live", "busy")
	writeSession(t, dir, 999999, "dead", "busy")

	sessions, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(sessions) != 1 || sessions[0].SessionID != "live" {
		t.Errorf("sessions = %+v, want only the live one", sessions)
	}
}

func TestScanToleratesMissingDirectory(t *testing.T) {
	sessions, err := Scan(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(sessions) != 0 {
		t.Errorf("sessions = %+v, want none", sessions)
	}
}

func TestScanSkipsUnparseableEntries(t *testing.T) {
	dir := t.TempDir()

	writeSession(t, dir, os.Getpid(), "good", "idle")

	if err := os.WriteFile(filepath.Join(dir, "9.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing junk: %v", err)
	}

	sessions, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(sessions) != 1 {
		t.Errorf("len = %d, want 1", len(sessions))
	}
}

// Pane ids cannot be computed from an index, so the registry's own value is the
// authoritative one.
func TestPaneID(t *testing.T) {
	for _, tc := range []struct{ tmux, want string }{
		{"jarush:@4.%13", "%13"},
		{"session:@1.%137", "%137"},
		{"", ""},
		{"no-pane-here", ""},
	} {
		if got := (Session{Tmux: tc.tmux}).PaneID(); got != tc.want {
			t.Errorf("PaneID(%q) = %q, want %q", tc.tmux, got, tc.want)
		}
	}
}

func TestSessionUpdated(t *testing.T) {
	if got := (Session{}).Updated(); !got.IsZero() {
		t.Errorf("Updated() = %v, want the zero time", got)
	}

	if got := (Session{UpdatedAt: 1786975627255}).Updated(); got.IsZero() {
		t.Error("Updated() should decode a millisecond epoch")
	}
}

func TestBySessionID(t *testing.T) {
	index := BySessionID([]Session{{SessionID: "a"}, {SessionID: "b"}})

	if len(index) != 2 || index["a"].SessionID != "a" {
		t.Errorf("index = %+v", index)
	}
}
