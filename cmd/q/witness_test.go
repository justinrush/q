package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/azure"
	"github.com/justinrush/q/internal/k8s"
	"github.com/justinrush/q/internal/mission"
)

func TestDefaultsConsultNoWitness(t *testing.T) {
	s := defaultSettings()

	if s.Remote.Witness.Kubernetes != nil || s.Remote.Witness.Azure != nil {
		t.Errorf("default witness = %+v, want none", s.Remote.Witness)
	}

	if w, err := witnessFor(s); w != nil || err != nil {
		t.Errorf("witnessFor = %v, %v; want no witness and no error", w, err)
	}
}

func TestApplyFileConfigSetsTheWitness(t *testing.T) {
	s := defaultSettings()
	applyFile(&s, fileConfig{Remote: &remoteConfig{Witness: &witnessConfig{
		Kubernetes: &kubernetesWitnessConfig{Context: "home", Namespace: "q", Lease: "pair"},
	}}})

	if got := s.Remote.Witness.Kubernetes; got == nil || *got != (k8s.Config{Context: "home", Namespace: "q", Name: "pair"}) {
		t.Errorf("kubernetes witness = %+v", got)
	}

	s = defaultSettings()
	applyFile(&s, fileConfig{Remote: &remoteConfig{Witness: &witnessConfig{
		Azure: &azureWitnessConfig{Account: "acct", Container: "q", Blob: "pair.json"},
	}}})

	if got := s.Remote.Witness.Azure; got == nil || *got != (azure.Config{Account: "acct", Container: "q", Blob: "pair.json"}) {
		t.Errorf("azure witness = %+v", got)
	}

	// What was read is what a generated config writes back.
	if file := witnessFile(s.Remote.Witness); file == nil || file.Azure == nil || file.Azure.Account != "acct" || file.Kubernetes != nil {
		t.Errorf("witnessFile = %+v, want the azure witness and nothing else", file)
	}
}

func TestAPairHasOneWitness(t *testing.T) {
	s := defaultSettings()
	s.Remote.Witness = witnessSettings{Kubernetes: &k8s.Config{}, Azure: &azure.Config{Account: "acct"}}

	if _, err := witnessFor(s); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("witnessFor: %v, want both kinds at once refused", err)
	}
}

func TestWriteWitnessConfigKeepsEverythingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	original := `{"queue":{"maxConcurrent":3},"remote":{"ssh":["ssh","mini.local"],"takeoverAfter":"10m"}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	set := &witnessConfig{Azure: &azureWitnessConfig{Account: "acct", Container: "q", Blob: "witness.json"}}
	if err := writeWitnessConfig(path, set); err != nil {
		t.Fatalf("writeWitnessConfig: %v", err)
	}

	file, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("the rewritten file does not parse: %v", err)
	}

	if file.Remote.Witness == nil || file.Remote.Witness.Azure == nil || file.Remote.Witness.Azure.Account != "acct" {
		t.Errorf("remote.witness = %+v", file.Remote.Witness)
	}

	if file.Queue.MaxConcurrent != 3 || len(file.Remote.SSH) != 2 || file.Remote.TakeoverAfter != "10m" {
		t.Errorf("other settings were lost: %+v remote %+v", file, file.Remote)
	}

	// Clearing removes the witness and nothing else.
	if err := writeWitnessConfig(path, nil); err != nil {
		t.Fatalf("writeWitnessConfig: %v", err)
	}

	data, _ := os.ReadFile(path)
	file, _ = readConfigFile(path)

	if strings.Contains(string(data), "witness") || len(file.Remote.SSH) != 2 {
		t.Errorf("after clearing:\n%s", data)
	}
}

// fakeWitness answers a probe from memory.
type fakeWitness struct {
	claim    mission.Claim
	readErr  error
	writeErr error
	wrote    *mission.Claim
}

func (f *fakeWitness) Name() string { return "fake:pair" }

func (f *fakeWitness) Read(context.Context) (mission.Claim, error) { return f.claim, f.readErr }

func (f *fakeWitness) Write(_ context.Context, claim mission.Claim) error {
	f.wrote = &claim

	return f.writeErr
}

// Saving a witness this machine can read and not write would leave it unable
// to claim, which looks the same as never being vouched for.
func TestProbingAWitnessProvesItCanBeWritten(t *testing.T) {
	held := mission.Claim{Holder: "h_bbbbbbbbbbbb", Version: "7"}

	ok := &fakeWitness{claim: held}
	if got, err := probeWitness(t.Context(), ok); err != nil || got != held || ok.wrote == nil || *ok.wrote != held {
		t.Errorf("probe = %+v, %v, wrote %+v; want the claim read and written back unchanged", got, err, ok.wrote)
	}

	raced := &fakeWitness{claim: held, writeErr: mission.ErrClaimRaced}
	if _, err := probeWitness(t.Context(), raced); err != nil {
		t.Errorf("probe: %v, want losing a race to count as able to write", err)
	}

	forbidden := &fakeWitness{claim: held, writeErr: errors.New("forbidden")}
	if _, err := probeWitness(t.Context(), forbidden); err == nil {
		t.Error("a witness that cannot be written passed the probe")
	}

	unreachable := &fakeWitness{readErr: errors.New("no route to host")}
	if _, err := probeWitness(t.Context(), unreachable); err == nil || unreachable.wrote != nil {
		t.Errorf("probe: %v wrote %+v, want the read failure and no write", err, unreachable.wrote)
	}
}

func TestClaimLabel(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	peer := mission.HostInfo{ID: "h_bbbbbbbbbbbb", Name: "mini"}
	status := api.RemoteStatus{Self: mission.HostInfo{ID: "h_aaaaaaaaaaaa", Name: "laptop"}, Peer: &peer}

	cases := []struct {
		name  string
		claim mission.Claim
		want  []string
	}{
		{name: "unclaimed", want: []string{"nobody yet"}},
		{
			name:  "renewed by this machine",
			claim: mission.Claim{Holder: "h_aaaaaaaaaaaa", RenewedAt: now.Add(-time.Minute), TTL: 5 * time.Minute},
			want:  []string{"this machine", "renewed 1m0s ago", "another 4m0s"},
		},
		{
			name:  "lapsed",
			claim: mission.Claim{Holder: "h_aaaaaaaaaaaa", RenewedAt: now.Add(-time.Hour), TTL: 5 * time.Minute},
			want:  []string{"this machine", "lapsed 55m0s ago"},
		},
		{
			name:  "taken by the other machine",
			claim: mission.Claim{Holder: "h_bbbbbbbbbbbb", RenewedAt: now.Add(-time.Hour)},
			want:  []string{"mini (h_bbbbbbbbbbbb)", "until the two next speak"},
		},
		{
			name:  "somebody else's record",
			claim: mission.Claim{Holder: "h_cccccccccccc", RenewedAt: now, TTL: time.Minute},
			want:  []string{"h_cccccccccccc", "neither machine of this pair"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claimLabel(tc.claim, status, now)

			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("label %q lacks %q", got, want)
				}
			}
		})
	}
}

func TestDescribeRemoteExplainsTheWitness(t *testing.T) {
	peer := mission.HostInfo{ID: "h_bbbbbbbbbbbb", Name: "mini"}
	base := api.RemoteStatus{
		Self: mission.HostInfo{ID: "h_aaaaaaaaaaaa", Name: "laptop"}, Role: mission.RolePrimary, Peer: &peer,
	}

	cases := []struct {
		name    string
		witness api.WitnessStatus
		want    []string
	}{
		{
			name:    "vouched for",
			witness: api.WitnessStatus{Name: "w", PeerName: "w", Holder: "h_aaaaaaaaaaaa", AskedAt: time.Now()},
			want:    []string{"witness", "names this machine"},
		},
		{
			name:    "standing by behind the other machine",
			witness: api.WitnessStatus{Name: "w", PeerName: "w", Holder: "h_bbbbbbbbbbbb", AskedAt: time.Now(), Standby: true},
			want:    []string{"names mini", "standing by", "q remote witness take"},
		},
		{
			name:    "unreachable",
			witness: api.WitnessStatus{Name: "w", PeerName: "w", Error: "no route to host", AskedAt: time.Now()},
			want:    []string{"unreachable: no route to host"},
		},
		{
			name:    "only here",
			witness: api.WitnessStatus{Name: "w"},
			want:    []string{"other machine has not said", "nothing is taken over", "there"},
		},
		{
			name:    "only there",
			witness: api.WitnessStatus{PeerName: "w"},
			want:    []string{"none here", "nothing is taken over", "here"},
		},
		{
			name:    "two different ones",
			witness: api.WitnessStatus{Name: "w", PeerName: "x"},
			want:    []string{"but the other machine uses x", "same one"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := base
			status.Witness = &tc.witness

			rep := newReport()
			describeRemote(rep, status)

			for _, want := range tc.want {
				if !strings.Contains(rep.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, rep.String())
				}
			}
		})
	}
}
