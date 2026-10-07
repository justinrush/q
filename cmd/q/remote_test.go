package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/remote"
)

func TestRemoteDefaultsLeaveQUnpaired(t *testing.T) {
	s := defaultSettings()

	if len(s.Remote.SSH) != 0 {
		t.Errorf("default remote.ssh = %v, want none: q must not dial anything unasked", s.Remote.SSH)
	}

	if s.Remote.Bin != remote.DefaultBin || s.Remote.Interval <= 0 || s.Remote.TakeoverAfter <= 0 {
		t.Errorf("defaults = %+v, want a bin and positive intervals", s.Remote)
	}
}

func TestApplyFileConfigSetsRemote(t *testing.T) {
	s := defaultSettings()

	applyFile(&s, fileConfig{Remote: &remoteConfig{
		SSH:           []string{"ssh", "-J", "bastion", "dev-vm"},
		Bin:           "~/bin/q",
		Name:          "laptop",
		Interval:      "30s",
		TakeoverAfter: "10m",
	}})

	if !slices.Equal(s.Remote.SSH, []string{"ssh", "-J", "bastion", "dev-vm"}) || s.Remote.Name != "laptop" {
		t.Errorf("remote = %+v", s.Remote)
	}

	if s.Remote.Interval != 30*time.Second || s.Remote.TakeoverAfter != 10*time.Minute {
		t.Errorf("intervals = %s / %s, want 30s / 10m", s.Remote.Interval, s.Remote.TakeoverAfter)
	}

	// The bin is the other machine's path. Expanding ~ here would bake this
	// machine's home directory into a command that runs over there.
	expandSettings(&s)

	if s.Remote.Bin != "~/bin/q" {
		t.Errorf("remote.bin = %q after expansion, want it left for the peer's shell", s.Remote.Bin)
	}
}

// A bad duration must leave the default in place rather than zero, which would
// mean an exchange in a tight loop or a takeover the instant a laptop sleeps.
func TestApplyFileConfigIgnoresBadRemoteDurations(t *testing.T) {
	s := defaultSettings()
	want := s.Remote

	applyFile(&s, fileConfig{Remote: &remoteConfig{Interval: "soon", TakeoverAfter: "-5m"}})

	if s.Remote.Interval != want.Interval || s.Remote.TakeoverAfter != want.TakeoverAfter {
		t.Errorf("intervals = %s / %s, want the defaults kept", s.Remote.Interval, s.Remote.TakeoverAfter)
	}
}

func TestApplyEnvSetsRemote(t *testing.T) {
	s := defaultSettings()

	t.Setenv(EnvRemoteSSH, "ssh mini.local")
	t.Setenv(EnvHostName, "laptop")
	applyEnv(&s)

	if !slices.Equal(s.Remote.SSH, []string{"ssh", "mini.local"}) || s.Remote.Name != "laptop" {
		t.Errorf("remote = %+v", s.Remote)
	}
}

// Setup edits a file the user wrote by hand. It may change its formatting and
// nothing else.
func TestWriteRemoteConfigKeepsEverythingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	original := `{"editor":{"command":["nvim","+Neotree"]},"queue":{"maxConcurrent":3},"logLevel":"debug"}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeRemoteConfig(path, []string{"ssh", "mini.local"}, ""); err != nil {
		t.Fatalf("writeRemoteConfig: %v", err)
	}

	file, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("the rewritten file does not parse: %v", err)
	}

	if !slices.Equal(file.Remote.SSH, []string{"ssh", "mini.local"}) {
		t.Errorf("remote.ssh = %v", file.Remote.SSH)
	}

	if !slices.Equal(file.Editor.Command, []string{"nvim", "+Neotree"}) || file.Queue.MaxConcurrent != 3 || file.LogLevel != "debug" {
		t.Errorf("other settings were lost: %+v", file)
	}

	// Defaults must not be frozen into the file: only what was there, plus the
	// one key setup is for.
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "repos") || strings.Contains(string(data), "interval") {
		t.Errorf("setup wrote settings nobody chose:\n%s", data)
	}
}

func TestWriteRemoteConfigCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	if err := writeRemoteConfig(path, []string{"ssh", "mini.local"}, "/opt/q"); err != nil {
		t.Fatalf("writeRemoteConfig: %v", err)
	}

	file, err := readConfigFile(path)
	if err != nil || file.Remote == nil || file.Remote.Bin != "/opt/q" {
		t.Errorf("file = %+v, err = %v", file, err)
	}
}

func TestDescribeRemote(t *testing.T) {
	peer := mission.HostInfo{ID: "h_bbbbbbbbbbbb", Name: "mini"}

	cases := []struct {
		name   string
		status api.RemoteStatus
		want   []string
	}{
		{
			name:   "unpaired",
			status: api.RemoteStatus{Self: mission.HostInfo{ID: "h_aaaaaaaaaaaa", Name: "laptop"}},
			want:   []string{"laptop (h_aaaaaaaaaaaa)", "paired", "no", "q remote setup"},
		},
		{
			name:   "linked",
			status: api.RemoteStatus{Role: mission.RolePrimary, Peer: &peer, LastSyncAt: time.Now()},
			want:   []string{"primary", "mini (h_bbbbbbbbbbbb)", "up, last exchange"},
		},
		{
			name:   "down with work waiting",
			status: api.RemoteStatus{Role: mission.RolePrimary, Peer: &peer, Error: "connection timed out", Pending: 2},
			want:   []string{"down: connection timed out", "2 request(s) waiting"},
		},
		{
			name:   "never connected",
			status: api.RemoteStatus{Role: mission.RoleSecondary, Peer: &peer},
			want:   []string{"secondary", "no exchange since this daemon started"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := newReport()
			describeRemote(rep, tc.status)

			for _, want := range tc.want {
				if !strings.Contains(rep.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, rep.String())
				}
			}
		})
	}
}

// The relay runs with the user's shell access on the other machine. It must
// carry daemon API requests and nothing else.
func TestRPCRefusesAPathOutsideTheAPI(t *testing.T) {
	var out bytes.Buffer

	err := runRPC(t.Context(), strings.NewReader(`{"method":"GET","path":"/debug/pprof"}`), &out)
	if err == nil || !strings.Contains(err.Error(), "refusing to relay") {
		t.Errorf("err = %v, want a refusal", err)
	}

	if out.Len() != 0 {
		t.Errorf("wrote %q for a refused request", out.String())
	}
}

func TestRPCRejectsGarbage(t *testing.T) {
	var out bytes.Buffer

	if err := runRPC(t.Context(), strings.NewReader("not json"), &out); err == nil {
		t.Error("garbage on standard input was accepted")
	}
}

// One line, always: the caller finds the answer by its marker.
func TestCompactJSONStaysOnOneLine(t *testing.T) {
	got := compactJSON([]byte("{\"a\":1}\n"))
	if string(got) != `{"a":1}` {
		t.Errorf("compactJSON = %q", got)
	}

	if compactJSON([]byte("  \n")) != nil || compactJSON([]byte("<html>")) != nil {
		t.Error("an empty or non-JSON body should become no body")
	}
}
