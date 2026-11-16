package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// An unpaired q has no host to offer, and the field is neither drawn nor
// reached by tab.
func TestMissionFormOffersNoHostWhenUnpaired(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}
	form := newMissionForm(mission.Mission{}, operations, "op_1", Options{}, nil, time.Time{})

	if form.offersHost() || !form.hidden(fieldMissionHost) {
		t.Error("the host field is offered without a peer")
	}

	if strings.Contains(form.View(200, 60), "Runs on") {
		t.Error("the host field is drawn without a peer")
	}
}

// A paired q offers auto, here, and the peer, and the choice is submitted.
func TestMissionFormHostFieldCyclesAndSubmits(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}
	form := newMissionForm(mission.Mission{}, operations, "op_1", Options{}, nil, time.Time{})
	form.setHosts("h_laptop", "h_mini", "laptop", "mini", "")

	if !strings.Contains(form.View(200, 60), "Runs on") {
		t.Fatal("the host field is not drawn for a paired q")
	}

	if form.pin != "" {
		t.Fatalf("pin = %q, want auto to start", form.pin)
	}

	form.cycleHost(keyRight)
	form.cycleHost(keyRight)

	if form.pin != "remote" || !strings.Contains(form.hostValue(), "mini") {
		t.Fatalf("pin = %q (%s), want the mini after two steps", form.pin, form.hostValue())
	}

	form.name.SetValue("overnight")
	form.prompt.SetValue("do it")

	_, cmd := form.submit(true)
	if cmd == nil {
		t.Fatal("the form did not submit")
	}

	msg, ok := cmd().(submitMissionMsg)
	if !ok || msg.Pin != "remote" || !msg.HasPin {
		t.Errorf("submitted %+v, want the remote pin", msg)
	}
}

// An edit starts on the mission's own pin, and a launched mission is moved
// with send and take instead.
func TestMissionFormHostFieldFollowsTheMission(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}

	form := newMissionForm(mission.Mission{ID: "ms_1", Pin: "h_mini"}, operations, "op_1", Options{}, nil, time.Time{})
	form.setHosts("h_laptop", "h_mini", "laptop", "mini", "h_mini")

	if form.pin != "remote" {
		t.Errorf("pin = %q, want remote for a mission pinned to the peer", form.pin)
	}

	started := time.Now()
	launched := newMissionForm(mission.Mission{ID: "ms_2", StartedAt: &started, Status: mission.StatusActive},
		operations, "op_1", Options{}, nil, time.Time{})
	launched.setHosts("h_laptop", "h_mini", "laptop", "mini", "")

	if launched.offersHost() {
		t.Error("the host field is offered for a launched mission")
	}
}

// S on a card asks to send it.
func TestBoardSendKeyEmitsASend(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}
	board := boardWith(operations, []mission.Mission{
		{ID: "ms_1", OperationID: "op_1", Name: "job", Status: mission.StatusBriefing},
	})

	cmd := board.Update(keyMsg("S"))
	if cmd == nil {
		t.Fatal("S produced no command")
	}

	if msg, ok := cmd().(sendMissionMsg); !ok || msg.Mission.ID != "ms_1" {
		t.Errorf("S produced %T, want a send of the focused mission", cmd())
	}
}

// A card pinned to the peer says so beside the host.
func TestBoardMarksAPinnedCard(t *testing.T) {
	board := NewBoard()
	board.SetSnapshot(mission.Snapshot{
		Self: mission.HostInfo{ID: "h_laptop", Name: "laptop"},
		Peer: &mission.Peer{HostInfo: mission.HostInfo{ID: "h_mini", Name: "mini"}},
	})

	pinned := mission.Mission{ID: "ms_1", Pin: "h_mini", Status: mission.StatusBriefing}
	unpinned := mission.Mission{ID: "ms_2", Status: mission.StatusBriefing}

	if got := board.hostOf(pinned); got != "mini"+pinnedMark {
		t.Errorf("hostOf(pinned) = %q, want the host marked as pinned", got)
	}

	if got := board.hostOf(unpinned); got != "" {
		t.Errorf("hostOf(unpinned) = %q, want nothing", got)
	}
}
