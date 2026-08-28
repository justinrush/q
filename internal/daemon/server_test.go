package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/state"
)

const testToken = "test-token"

// newTestServer starts a real HTTP server on loopback and returns it with a
// helper client. httptest.Server is not used directly because the auth guard
// checks the Host header against the address we bound.
func newTestServer(t *testing.T) (*Server, *http.Client, string) {
	t.Helper()

	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	store, err := state.Open(dirs)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	hub := NewHub()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := NewService(ServiceConfig{Store: store, Hub: hub, Dirs: dirs, Logger: logger, Now: time.Now})

	queue := newHookQueue()
	go queue.run(svc)

	t.Cleanup(queue.close)

	srv := NewServer(Config{
		Service: svc,
		Hub:     hub,
		Queue:   queue,
		Dirs:    dirs,
		Token:   testToken,
		Version: "test",
		Logger:  logger,
	})

	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	go func() { _ = srv.http.Serve(srv.listener) }()

	t.Cleanup(func() {
		hub.Close()
		_ = srv.http.Close()
	})

	return srv, &http.Client{}, "http://" + addr
}

// do issues an authenticated request.
func do(t *testing.T, c *http.Client, method, url string, body any) *http.Response {
	t.Helper()

	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding body: %v", err)
		}

		reader = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(t.Context(), method, url, reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(api.ClientHeader, api.ClientHeaderValue)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	return resp
}

// decodeBody unmarshals a response body into dst.
func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()

	defer func() { _ = resp.Body.Close() }()

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

// The token is the real control, but each layer is checked independently so a
// regression in one is visible.
func TestAuthMatrix(t *testing.T) {
	_, c, base := newTestServer(t)

	cases := []struct {
		name    string
		mutate  func(*http.Request)
		wantOK  bool
		comment string
	}{
		{
			name:    "fully authenticated",
			mutate:  func(*http.Request) {},
			wantOK:  true,
			comment: "the happy path",
		},
		{
			name:   "no authorization header",
			mutate: func(r *http.Request) { r.Header.Del("Authorization") },
		},
		{
			name:   "wrong token",
			mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
		},
		{
			name:   "token without bearer prefix",
			mutate: func(r *http.Request) { r.Header.Set("Authorization", testToken) },
		},
		{
			name:   "missing client header",
			mutate: func(r *http.Request) { r.Header.Del(api.ClientHeader) },
		},
		{
			name:   "wrong client header value",
			mutate: func(r *http.Request) { r.Header.Set(api.ClientHeader, "0") },
		},
		{
			// A rebinding guard: a DNS name resolving to loopback must not pass.
			name:   "foreign host header",
			mutate: func(r *http.Request) { r.Host = "evil.example.com" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/health", nil)
			if err != nil {
				t.Fatalf("building request: %v", err)
			}

			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set(api.ClientHeader, api.ClientHeaderValue)
			tc.mutate(req)

			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if tc.wantOK && resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}

			if !tc.wantOK && resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestHealthReportsCounts(t *testing.T) {
	_, c, base := newTestServer(t)

	resp := do(t, c, http.MethodPost, base+"/v1/operations", api.CreateOperationRequest{Name: "Operation"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create operation status = %d", resp.StatusCode)
	}

	_ = resp.Body.Close()

	var health api.Health
	decodeBody(t, do(t, c, http.MethodGet, base+"/v1/health", nil), &health)

	if health.Operations != 1 {
		t.Errorf("Operations = %d, want 1", health.Operations)
	}

	if health.Version != "test" {
		t.Errorf("Version = %q, want %q", health.Version, "test")
	}
}

func TestOperationAndMissionLifecycleOverHTTP(t *testing.T) {
	_, c, base := newTestServer(t)

	var operation domain.Operation
	decodeBody(t, do(t, c, http.MethodPost, base+"/v1/operations", api.CreateOperationRequest{
		Name:    "Discussions API",
		Summary: "wire discussions through",
	}), &operation)

	if operation.Slug != "discussions-api" {
		t.Errorf("Slug = %q, want %q", operation.Slug, "discussions-api")
	}

	var mission domain.Mission
	decodeBody(t, do(t, c, http.MethodPost, base+"/v1/missions", api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "add endpoint",
		Prompt:      "do the thing",
		Tool:        domain.ToolClaude,
	}), &mission)

	// New missions always land in draft; launching is a separate explicit action.
	if mission.Status != domain.StatusBriefing {
		t.Errorf("Status = %q, want %q", mission.Status, domain.StatusBriefing)
	}

	var moved domain.Mission
	decodeBody(t, do(t, c, http.MethodPost, base+"/v1/missions/"+string(mission.ID)+"/status",
		api.SetStatusRequest{To: domain.StatusDebrief}), &moved)

	if moved.Status != domain.StatusDebrief {
		t.Errorf("Status = %q, want %q", moved.Status, domain.StatusDebrief)
	}

	// Deleting returns a report of what it reclaimed rather than an empty response,
	// because a branch kept behind is something the caller has to be told about.
	resp := do(t, c, http.MethodDelete, base+"/v1/missions/"+string(mission.ID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d, want 200", resp.StatusCode)
	}

	_ = resp.Body.Close()
}

func TestDoneStatusReclaimsResourcesAndRetainsCard(t *testing.T) {
	srv, c, base := newTestServer(t)
	mission := launchedServiceMission(t, srv.svc)
	reclaimer := &fakeReclaimer{report: launch.Report{Removed: []string{"/missions/mission/repo"}}}
	srv.svc.SetReclaimer(reclaimer)

	var finished domain.Mission
	resp := do(t, c, http.MethodPost, base+"/v1/missions/"+string(mission.ID)+"/status",
		api.SetStatusRequest{To: domain.StatusClosed})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finish status = %d, want 200", resp.StatusCode)
	}

	decodeBody(t, resp, &finished)

	if reclaimer.calls != 1 {
		t.Fatalf("reclaim calls = %d, want 1", reclaimer.calls)
	}

	if finished.Status != domain.StatusClosed || finished.MissionDir != "" {
		t.Errorf("finished mission = %+v", finished)
	}

	if _, ok := srv.svc.Snapshot().Mission(mission.ID); !ok {
		t.Error("the done card should remain in state")
	}
}

func TestDoneStatusRequiresForceForDirtyWork(t *testing.T) {
	srv, c, base := newTestServer(t)
	mission := launchedServiceMission(t, srv.svc)
	reclaimer := &fakeReclaimer{err: launch.ErrNeedsForce}
	srv.svc.SetReclaimer(reclaimer)

	resp := do(t, c, http.MethodPost, base+"/v1/missions/"+string(mission.ID)+"/status",
		api.SetStatusRequest{To: domain.StatusClosed})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("finish status = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()

	stored, ok := srv.svc.Snapshot().Mission(mission.ID)
	if !ok || stored.Status == domain.StatusClosed {
		t.Errorf("a refused finish changed the mission: %+v", stored)
	}
}

func TestDoneStatusPassesConfirmedForce(t *testing.T) {
	srv, c, base := newTestServer(t)
	mission := launchedServiceMission(t, srv.svc)
	reclaimer := &fakeReclaimer{}
	srv.svc.SetReclaimer(reclaimer)

	resp := do(t, c, http.MethodPost, base+"/v1/missions/"+string(mission.ID)+"/status",
		api.SetStatusRequest{To: domain.StatusClosed, Force: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finish status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if !reclaimer.force {
		t.Error("the confirmed force flag did not reach reclamation")
	}
}

func TestErrorStatusMapping(t *testing.T) {
	_, c, base := newTestServer(t)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"unknown operation", http.MethodPatch, "/v1/operations/op_000000000000", api.UpdateOperationRequest{}, http.StatusNotFound},
		{"unknown mission", http.MethodDelete, "/v1/missions/ms_000000000000", nil, http.StatusNotFound},
		{"nameless operation", http.MethodPost, "/v1/operations", api.CreateOperationRequest{}, http.StatusBadRequest},
		{
			"mission on unknown operation", http.MethodPost, "/v1/missions",
			api.CreateMissionRequest{OperationID: "op_000000000000", Name: "x", Prompt: "y"},
			http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, c, tc.method, base+tc.path, tc.body)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	_, c, base := newTestServer(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/operations",
		strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(api.ClientHeader, api.ClientHeaderValue)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A reconnecting client must resynchronize without asking, which is what makes
// dropping a slow subscriber safe.
func TestEventStreamSendsSnapshotThenChanges(t *testing.T) {
	_, c, base := newTestServer(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/events", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(api.ClientHeader, api.ClientHeaderValue)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("opening stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	reader := bufio.NewReader(resp.Body)

	name, payload := readFrame(t, reader)
	if name != EventSnapshot {
		t.Fatalf("first event = %q, want %q", name, EventSnapshot)
	}

	var snap state.Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}

	// Now provoke a change and confirm it arrives on the open stream.
	created := do(t, c, http.MethodPost, base+"/v1/operations", api.CreateOperationRequest{Name: "Live"})
	_ = created.Body.Close()

	name, payload = readFrame(t, reader)
	if name != EventOperation {
		t.Fatalf("second event = %q, want %q", name, EventOperation)
	}

	var operation domain.Operation
	if err := json.Unmarshal(payload, &operation); err != nil {
		t.Fatalf("decoding operation: %v", err)
	}

	if operation.Name != "Live" {
		t.Errorf("operation Name = %q, want %q", operation.Name, "Live")
	}
}

// readFrame parses one SSE frame.
func readFrame(t *testing.T, reader *bufio.Reader) (string, []byte) {
	t.Helper()

	var name, data string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading frame: %v", err)
		}

		line = strings.TrimRight(line, "\r\n")

		switch {
		case line == "":
			if data != "" {
				return name, []byte(data)
			}
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// The reducer must never block on a client, so a subscriber that stops reading is
// dropped rather than waited on.
func TestHubDropsSlowSubscriber(t *testing.T) {
	hub := NewHub()
	defer hub.Close()

	_, frames := hub.Subscribe()

	for range subBuffer + 10 {
		hub.Broadcast(EventMission, domain.Mission{Name: "flood"})
	}

	if got := hub.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d, want 0 after overflow", got)
	}

	// Draining must terminate: the channel is closed when the subscriber is
	// dropped, so a reader is not left blocked forever.
	drained := 0
	for range frames {
		drained++
	}

	if drained == 0 {
		t.Error("expected buffered frames to remain readable after the drop")
	}
}

func TestHubUnsubscribeClosesChannel(t *testing.T) {
	hub := NewHub()
	defer hub.Close()

	id, frames := hub.Subscribe()
	hub.Unsubscribe(id)

	if _, open := <-frames; open {
		t.Error("channel should be closed after Unsubscribe")
	}
}

func TestHubBroadcastIsConcurrencySafe(t *testing.T) {
	hub := NewHub()
	defer hub.Close()

	var wg sync.WaitGroup

	for range 4 {
		_, frames := hub.Subscribe()

		wg.Go(func() {
			for range frames {
			}
		})
	}

	for range 4 {
		wg.Go(func() {
			for range 50 {
				hub.Broadcast(EventMission, domain.Mission{Name: "x"})
			}
		})
	}

	hub.Close()
	wg.Wait()
}

// SSE is line-oriented, so an embedded newline in the payload would split one
// event into two malformed ones.
func TestEncodeFrameKeepsPayloadOnOneLine(t *testing.T) {
	frame, err := encodeFrame(EventMission, domain.Mission{
		Name:   "multi\nline",
		Prompt: "first\nsecond\r\nthird",
	})
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}

	text := string(frame)
	if !strings.HasSuffix(text, "\n\n") {
		t.Errorf("frame must end with a blank line: %q", text)
	}

	body := strings.TrimSuffix(text, "\n\n")
	if strings.Count(body, "\n") != 1 {
		t.Errorf("frame body should hold exactly one newline between event and data: %q", body)
	}
}

// The hook path carries the event as a slug while the state machine dispatches on
// canonical names. Failing to convert makes every event fall through to the default
// case and silently do nothing, which defeats the whole status pipeline without any
// error appearing anywhere.
func TestHookEndpointCanonicalizesEventSlug(t *testing.T) {
	_, c, base := newTestServer(t)

	var operation domain.Operation
	decodeBody(t, do(t, c, http.MethodPost, base+"/v1/operations", api.CreateOperationRequest{Name: "T"}), &operation)

	var mission domain.Mission
	decodeBody(t, do(t, c, http.MethodPost, base+"/v1/missions", api.CreateMissionRequest{
		OperationID: operation.ID, Name: "mission", Prompt: "x",
	}), &mission)

	payload := `{"session_id":"sess-1","cwd":"/tmp","transcript_path":"/tmp/t.jsonl",` +
		`"hook_event_name":"SessionStart","source":"startup"}`

	resp := do(t, c, http.MethodPost, base+"/v1/hooks/claude/session-start", api.HookRequest{
		MissionID: mission.ID,
		HookEpoch: 1,
		Payload:   json.RawMessage(payload),
	})

	// A hook must always be told it succeeded, whatever q makes of it.
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	_ = resp.Body.Close()

	waitFor(t, func() bool {
		stored, ok := s(t, base, c).Mission(mission.ID)

		return ok && stored.AgentState == domain.AgentBusy
	}, "SessionStart to be applied")
}

// A hook must never learn about q's problems, because in claude a failing hook
// can block the tool call that triggered it.
func TestHookEndpointAlwaysReportsSuccess(t *testing.T) {
	_, c, base := newTestServer(t)

	for _, tc := range []struct{ name, path, body string }{
		{"unknown tool", "/v1/hooks/cursor/stop", `{}`},
		{"unknown event", "/v1/hooks/claude/not-an-event", `{}`},
		{"unparseable payload", "/v1/hooks/claude/stop", `{"payload":"not json"}`},
		{"unknown mission", "/v1/hooks/claude/stop", `{"missionId":"ms_000000000000","payload":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+tc.path,
				strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("building request: %v", err)
			}

			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set(api.ClientHeader, api.ClientHeaderValue)

			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("status = %d, want 204", resp.StatusCode)
			}
		})
	}
}

// s fetches the current snapshot.
func s(t *testing.T, base string, c *http.Client) state.Snapshot {
	t.Helper()

	var snap state.Snapshot
	decodeBody(t, do(t, c, http.MethodGet, base+"/v1/state", nil), &snap)

	return snap
}

// waitFor polls until cond holds, since hooks are reduced asynchronously.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

func TestValidHost(t *testing.T) {
	const addr = "127.0.0.1:8080"

	for _, tc := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"127.0.0.1:9999", false},
		{"example.com:8080", false},
		{"127.0.0.1", false},
		{"", false},
	} {
		if got := validHost(tc.host, addr); got != tc.want {
			t.Errorf("validHost(%q, %q) = %v, want %v", tc.host, addr, got, tc.want)
		}
	}
}

func TestLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"[::1]:1234", true},
		{"192.168.1.10:1234", false},
		{"nonsense", false},
	} {
		if got := loopback(tc.addr); got != tc.want {
			t.Errorf("loopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
