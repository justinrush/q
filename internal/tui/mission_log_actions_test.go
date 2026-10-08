package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

func TestLogCopyPreservesFullContents(t *testing.T) {
	body := "command\twith args\n" + strings.Repeat("diagnostic line\n", 100) + "last line\n"
	log := newMissionLog(mission.Mission{LaunchError: body})
	log.View(40, 15)
	log.Update(keyMsg("G"))
	next, cmd := log.Update(keyMsg("c"))
	if next != log || cmd().(copyMissionLogMsg).Log.body != body {
		t.Fatal("copy lost or wrapped log contents")
	}
	run := runner.NewFake()
	if err := copyLogNative(context.Background(), run, body); err != nil {
		t.Fatal(err)
	}
	if err := copyLogTmux(context.Background(), run, "/bin/tmux", body); err != nil {
		t.Fatal(err)
	}
	calls := run.Calls()
	if len(calls) != 2 || calls[0].Name != "/usr/bin/pbcopy" || calls[1].String() != "/bin/tmux load-buffer -w -" {
		t.Fatalf("unexpected clipboard commands: %v", calls)
	}
	for _, call := range calls {
		if string(call.Stdin) != body {
			t.Fatal("clipboard stdin lost contents")
		}
	}
	failure := errors.New("clipboard unavailable")
	run.ExpectError("/usr/bin/pbcopy", failure)
	if err := copyLogNative(context.Background(), run, body); !errors.Is(err, failure) {
		t.Fatal("clipboard error swallowed")
	}
	var out bytes.Buffer
	seq := &clipboardSequence{body: body}
	seq.SetStdout(&out)
	if err := seq.Run(); err != nil {
		t.Fatal(err)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(body)) + "\x07"
	if out.String() != want {
		t.Fatal("OSC 52 lost log contents")
	}
}

func TestTroubleshootLogQueuesInheritedMissionAndReportsResult(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			source := mission.Mission{ID: "ms_source", Name: "Broken launch", OperationID: "op_source", Tool: mission.ToolCodex, Model: "model", Effort: "high", Prompt: "Original brief", LaunchError: "fetch failed\nlast diagnostic", MissionDir: "/remote/mission", BaseBranches: map[string]string{"q": "feature"}, Lease: mission.Lease{Holder: "remote"}}
			var request api.CreateMissionRequest
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/missions" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if fail {
					http.Error(w, "queue unavailable", http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(mission.Mission{ID: "ms_child", Name: request.Name, Queued: request.Queued})
			}))
			defer server.Close()
			app := New(api.NewClient(api.Handle{Addr: strings.TrimPrefix(server.URL, "http://")}), Options{})
			app.snapshot = mission.Snapshot{Operations: []mission.Operation{{ID: "op_source", Summary: "Operation brief", Repos: []mission.Repo{{Name: "q", Path: "/repos/q"}}}}}
			log := newMissionLog(source)
			app.modal = log
			cmd := app.handleKey(keyMsg("t"))
			if cmd == nil || !log.queuing {
				t.Fatal("t did not queue")
			}
			if app.handleKey(keyMsg("t")) != nil {
				t.Fatal("duplicate key queued twice")
			}
			result := app.handleIntent(cmd())().(missionLogResultMsg)
			app.handleIntent(result)
			if requests != 1 || !request.Queued || request.InheritFrom != source.ID || request.Name != "Troubleshoot Broken launch" {
				t.Fatalf("wrong queued request: %+v", request)
			}
			for _, text := range []string{source.LaunchError, source.Prompt, source.MissionDir, "remote", "Operation brief", "/repos/q", "feature", "model", "high"} {
				if !strings.Contains(request.Prompt, text) {
					t.Errorf("missing context %q", text)
				}
			}
			if log.queuing || result.Err != fail || log.queued == fail {
				t.Fatal("wrong completion state")
			}
			if !strings.Contains(log.View(120, 24), map[bool]string{false: "Queued Troubleshoot", true: "Could not queue"}[fail]) {
				t.Fatal("result hidden in modal")
			}
			retry := app.handleKey(keyMsg("t"))
			if (retry != nil) != fail {
				t.Fatal("success allowed duplicate or failure prevented retry")
			}
		})
	}
}
