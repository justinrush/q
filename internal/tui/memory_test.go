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

func TestMemoryFinishToastAndExplicitSkip(t *testing.T) {
	var request api.SetStatusRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(mission.Mission{Name: "Sync", Status: mission.StatusDebrief, Finish: mission.FinishCapture{Token: "token", Phase: mission.FinishQueued}})
	}))
	defer server.Close()
	a := New(api.NewClient(api.Handle{Addr: strings.TrimPrefix(server.URL, "http://")}), Options{})
	msg := a.setStatusRequest("ms_test", api.SetStatusRequest{To: mission.StatusClosed, SkipMemory: true, Force: true})()
	toast, ok := msg.(toastMsg)
	if !ok || !strings.Contains(toast.text, "finish requested") || strings.Contains(toast.text, "resources reclaimed") {
		t.Fatalf("wrong pending result: %+v", msg)
	}
	if !request.SkipMemory || !request.Force {
		t.Fatal("skip lost cleanup authorization or capture choice")
	}
}

func TestMemoryFinishRecoveryActions(t *testing.T) {
	a := New(nil, Options{})
	ms := launchedMission(mission.ToolClaude, mission.StatusDebrief)
	ms.Finish = mission.FinishCapture{Token: "token", Phase: mission.FinishFailed, Error: "save failed"}
	if cmd := a.moveToLane(ms, mission.StatusClosed); cmd != nil {
		t.Fatal("failed capture should offer recovery")
	}
	modal, ok := a.modal.(*listModal)
	if !ok || len(modal.items) != 3 {
		t.Fatalf("missing recovery menu: %+v", a.modal)
	}
	for _, action := range []string{"retry", "skip", "cancel"} {
		found := false
		for _, item := range modal.items {
			found = found || item.Key == action
		}
		if !found {
			t.Fatal("missing action", action)
		}
	}
}
