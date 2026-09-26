package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// The daemon may have been started by another SSH client. It must never choose
// which terminal to attach when this board is using the current terminal.
func TestForegroundOpenOnlyPreparesOnDaemon(t *testing.T) {
	for _, tc := range []struct {
		name, bin, mode, wantMode string
		attach, steal             bool
	}{
		{"current", "tmux", api.DebriefAttach, api.DebriefPrepare, true, false},
		{"steal", "tmux", api.DebriefSteal, api.DebriefPrepare, true, true},
		{"prepare", "tmux", api.DebriefPrepare, api.DebriefPrepare, false, false},
		{"window", "", api.DebriefAttach, api.DebriefAttach, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mode string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req api.OpenDebriefRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				mode = req.Mode
				_ = json.NewEncoder(w).Encode(api.Result{Session: "q-mission"})
			}))
			defer server.Close()
			a := New(api.NewClient(api.Handle{Addr: strings.TrimPrefix(server.URL, "http://")}), Options{AttachBin: tc.bin})
			msg, ok := a.openDebriefCmd(mission.Mission{ID: "ms_test"}, tc.mode)().(debriefOpenedMsg)
			if !ok {
				t.Fatal("expected prepared mission")
			}
			if mode != tc.wantMode || msg.Attach != tc.attach || msg.Steal != tc.steal {
				t.Fatalf("mode=%q attach=%v steal=%v", mode, msg.Attach, msg.Steal)
			}
		})
	}
}

func TestForegroundOpenMissingSessionOffersRelaunch(t *testing.T) {
	a := New(nil, Options{AttachBin: "tmux"})
	cmd := a.handleDebriefOpened(debriefOpenedMsg{
		Mission: mission.Mission{Name: "ended"},
		Result:  api.Result{NeedsRelaunch: true},
		Attach:  true,
	})
	if cmd != nil || a.modal == nil {
		t.Fatal("missing session should offer relaunch without executing tmux")
	}
}
