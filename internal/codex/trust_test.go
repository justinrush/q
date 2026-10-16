package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTrustProjectWritesOnlyCanonicalMissionKey(t *testing.T) {
	directory := filepath.Join(t.TempDir(), `mission.with "quotes"`)
	err := os.Mkdir(directory, 0700)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked")
	err = os.Symlink(directory, link)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	stream := newScriptedStream("{\"id\":1,\"result\":{}}\n")
	client := NewClient(stream, stream)
	go func() { _ = client.Run(t.Context(), make(chan Notification)) }()
	err = (&Proxy{client: client}).TrustProject(t.Context(), link)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Method string            `json:"method"`
		Params map[string]string `json:"params"`
	}
	err = json.Unmarshal(stream.output.Bytes(), &request)
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != "config/value/write" || len(request.Params) != 3 || request.Params["keyPath"] != fmt.Sprintf("projects.%q.trust_level", canonical) || request.Params["value"] != "trusted" || request.Params["mergeStrategy"] != "upsert" {
		t.Fatalf("unexpected write: %+v", request)
	}
}

func TestTrustProjectRejectsMissingDirectory(t *testing.T) {
	err := (&Proxy{}).TrustProject(t.Context(), filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("missing directory accepted")
	}
}
