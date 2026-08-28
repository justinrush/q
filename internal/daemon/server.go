package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/spool"
)

// Server timeouts and limits.
const (
	// readHeaderTimeout bounds slow-header attacks. Required rather than
	// optional: gosec flags a Server without it.
	readHeaderTimeout = 5 * time.Second
	// maxBodyBytes caps request bodies. Hook payloads carry transcripts paths and
	// assistant messages, not transcripts themselves.
	maxBodyBytes = 1 << 20
	// pingInterval is how often an idle event stream emits a heartbeat, so a
	// client can detect a socket that died without notice.
	pingInterval = 15 * time.Second
)

// Server is the daemon's HTTP interface.
type Server struct {
	svc     *Service
	hub     *Hub
	queue   *hookQueue
	dirs    paths.Dirs
	token   string
	version string
	started time.Time
	logger  *slog.Logger

	// addr is the resolved listen address, known only after Listen.
	addr     string
	http     *http.Server
	listener net.Listener
}

// Config configures a Server.
type Config struct {
	Service *Service
	Hub     *Hub
	Queue   *hookQueue
	Dirs    paths.Dirs
	Token   string
	Version string
	Logger  *slog.Logger
	Now     func() time.Time
}

// NewServer builds a Server. Call [Server.Listen] then [Server.Serve].
func NewServer(cfg Config) *Server {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{
		svc:     cfg.Service,
		hub:     cfg.Hub,
		queue:   cfg.Queue,
		dirs:    cfg.Dirs,
		token:   cfg.Token,
		version: cfg.Version,
		started: now(),
		logger:  logger,
	}
}

// Listen binds an ephemeral loopback port and returns the resolved address.
//
// Port zero plus a read-back is what lets several users, or a test suite, run
// daemons side by side without coordinating on a fixed port.
func (s *Server) Listen() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listening on loopback: %w", err)
	}

	s.addr = ln.Addr().String()
	s.http = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		// WriteTimeout is deliberately unset: the event stream is a long-lived
		// response and any write deadline would sever it.
	}

	s.listener = ln

	return s.addr, nil
}

// Serve handles requests until ctx is canceled.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("Listen must be called before Serve")
	}

	errCh := make(chan error, 1)

	go func() {
		err := s.http.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		s.hub.Close()

		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}

		return nil
	}
}

// Addr returns the bound address.
func (s *Server) Addr() string { return s.addr }

// routes builds the request multiplexer.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/state", s.handleState)
	mux.HandleFunc("GET /v1/events", s.handleEvents)

	mux.HandleFunc("GET /v1/operations", s.handleListOperations)
	mux.HandleFunc("POST /v1/operations", s.handleCreateOperation)
	mux.HandleFunc("PATCH /v1/operations/{id}", s.handleUpdateOperation)
	mux.HandleFunc("DELETE /v1/operations/{id}", s.handleDeleteOperation)

	mux.HandleFunc("GET /v1/missions", s.handleListMissions)
	mux.HandleFunc("POST /v1/missions", s.handleCreateMission)
	mux.HandleFunc("PATCH /v1/missions/{id}", s.handleUpdateMission)
	mux.HandleFunc("DELETE /v1/missions/{id}", s.handleDeleteMission)
	mux.HandleFunc("POST /v1/missions/{id}/status", s.handleSetStatus)

	mux.HandleFunc("POST /v1/missions/{id}/open", s.handleOpenDebrief)
	mux.HandleFunc("POST /v1/missions/{id}/message", s.handleMessage)
	mux.HandleFunc("GET /v1/missions/{id}/diff", s.handleDiff)
	mux.HandleFunc("GET /v1/missions/{id}/delete-plan", s.handleDeletePlan)

	mux.HandleFunc("POST /v1/hooks/{tool}/{event}", s.handleHook)

	return s.guard(mux)
}

func (s *Server) handleOpenDebrief(w http.ResponseWriter, r *http.Request) {
	var req api.OpenDebriefRequest
	if !decode(w, r, &req) {
		return
	}

	mode := api.Mode(req.Mode)
	if mode == "" {
		mode = api.ModeAttach
	}

	result, err := s.svc.OpenDebrief(r.Context(), mission.MissionID(r.PathValue("id")), mode)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleMessage delivers text to a mission's live agent, reviving a dead session first.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req api.MessageRequest
	if !decode(w, r, &req) {
		return
	}

	ms, err := s.svc.Resume(r.Context(), mission.MissionID(r.PathValue("id")), req.Text)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, ms)
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	touched, err := s.svc.Diff(r.Context(), mission.MissionID(r.PathValue("id")))
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, touched)
}

// handleHook accepts an agent hook event.
//
// This is the only endpoint that must answer instantly. A hook holds up the agent
// while it runs, and in claude a hook that fails can block a tool call outright, so
// the event is queued without blocking and the response is always 204. If the
// reducer is saturated the event goes to the spool instead, which the daemon drains
// on its next start.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	tool, err := mission.ParseTool(r.PathValue("tool"))
	if err != nil {
		// Still 204: an agent can do nothing useful with a rejection.
		s.logger.Warn("hook from an unknown tool", "tool", r.PathValue("tool"))
		w.WriteHeader(http.StatusNoContent)

		return
	}

	var req api.HookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Warn("decoding a hook", "tool", tool, "error", err)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	req.Tool = tool

	// The path carries the event as a slug ("session-start"), while the state
	// machine dispatches on canonical names ("SessionStart"). Converting here is
	// required, not cosmetic: an unconverted slug matches no case and the event is
	// silently discarded.
	event, err := mission.CanonicalHookEvent(r.PathValue("event"))
	if err != nil {
		s.logger.Warn("hook for an unknown event", "event", r.PathValue("event"))
		w.WriteHeader(http.StatusNoContent)

		return
	}

	req.Event = event

	if s.queue == nil || !s.queue.offer(req) {
		if err := spool.Write(s.dirs.SpoolDir(), spool.Entry{ObservedAt: time.Now(), Hook: req}); err != nil {
			s.logger.Warn("spooling a hook", "tool", tool, "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// guard applies the three local-only authentication layers to every request.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(r); err != nil {
			writeError(w, http.StatusForbidden, err)

			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		next.ServeHTTP(w, r)
	})
}

// authorize checks the bearer token, that the peer is on loopback, and that the
// request carries the q client header.
//
// The token is the real control; the other two are defense in depth against a
// browser on the same machine being used to reach a localhost service.
func (s *Server) authorize(r *http.Request) error {
	if !loopback(r.RemoteAddr) {
		return errors.New("requests are only accepted from loopback")
	}

	if !validHost(r.Host, s.addr) {
		return errors.New("unexpected Host header")
	}

	if r.Header.Get(api.ClientHeader) != api.ClientHeaderValue {
		return fmt.Errorf("missing %s header", api.ClientHeader)
	}

	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errors.New("missing bearer token")
	}

	// Constant-time comparison so a caller cannot learn the token byte by byte
	// from response timing.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) != 1 {
		return errors.New("invalid token")
	}

	return nil
}

// loopback reports whether addr's host is a loopback address.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// validHost guards against DNS rebinding by requiring the Host header to name
// loopback explicitly on the port we bound.
func validHost(host, addr string) bool {
	if host == addr {
		return true
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	hostName, hostPort, err := net.SplitHostPort(host)
	if err != nil || hostPort != port {
		return false
	}

	if hostName == "localhost" {
		return true
	}

	ip := net.ParseIP(hostName)

	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := s.svc.Snapshot()

	self, err := os.Executable()
	if err != nil {
		self = "unknown"
	}

	writeJSON(w, http.StatusOK, api.Health{
		Version:     s.version,
		PID:         os.Getpid(),
		StartedAt:   s.started,
		Uptime:      time.Since(s.started).Round(time.Second).String(),
		Binary:      self,
		Operations:  len(snap.Operations),
		Missions:    len(snap.Missions),
		Subscribers: s.hub.Subscribers(),
	})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Snapshot())
}

// handleEvents streams state changes as server-sent events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id, frames := s.hub.Subscribe()
	defer s.hub.Unsubscribe(id)

	// Send the full state first so a reconnecting client resynchronizes without
	// needing to ask, which is what makes dropping slow subscribers safe.
	if frame, err := encodeFrame(api.EventSnapshot, s.svc.Snapshot()); err == nil {
		if _, err := w.Write(frame); err != nil {
			return
		}

		flusher.Flush()
	}

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-frames:
			if !open {
				return
			}

			if _, err := w.Write(frame); err != nil {
				return
			}

			flusher.Flush()
		case <-ticker.C:
			if frame, err := encodeFrame(api.EventPing, struct{}{}); err == nil {
				if _, err := w.Write(frame); err != nil {
					return
				}

				flusher.Flush()
			}
		}
	}
}

func (s *Server) handleListOperations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Snapshot().Operations)
}

func (s *Server) handleCreateOperation(w http.ResponseWriter, r *http.Request) {
	var req api.CreateOperationRequest
	if !decode(w, r, &req) {
		return
	}

	operation, err := s.svc.CreateOperation(req)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusCreated, operation)
}

func (s *Server) handleUpdateOperation(w http.ResponseWriter, r *http.Request) {
	var req api.UpdateOperationRequest
	if !decode(w, r, &req) {
		return
	}

	operation, err := s.svc.UpdateOperation(mission.OperationID(r.PathValue("id")), req)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, operation)
}

func (s *Server) handleDeleteOperation(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"

	if err := s.svc.DeleteOperation(mission.OperationID(r.PathValue("id")), force); err != nil {
		writeServiceError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListMissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Snapshot().Missions)
}

func (s *Server) handleCreateMission(w http.ResponseWriter, r *http.Request) {
	var req api.CreateMissionRequest
	if !decode(w, r, &req) {
		return
	}

	ms, err := s.svc.CreateMission(req)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusCreated, ms)
}

func (s *Server) handleUpdateMission(w http.ResponseWriter, r *http.Request) {
	var req api.UpdateMissionRequest
	if !decode(w, r, &req) {
		return
	}

	ms, err := s.svc.UpdateMission(mission.MissionID(r.PathValue("id")), req)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, ms)
}

// handleDeleteMission removes a mission and reclaims what it provisioned.
//
// Without force, a worktree holding uncommitted work makes this a conflict rather than a
// silent discard, so the caller has to decide.
func (s *Server) handleDeleteMission(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"

	report, err := s.svc.DeleteMissionAndReclaim(r.Context(), mission.MissionID(r.PathValue("id")), force)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, report)
}

// handleDeletePlan reports what deleting a mission would discard.
func (s *Server) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	plan, err := s.svc.PlanDelete(r.Context(), mission.MissionID(r.PathValue("id")))
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, plan)
}

// dispatchStatus decides what a lane move actually does.
//
// Moving out of draft launches the agent. Moving into the active lane from a lane that
// implies the agent is waiting resumes it, delivering the accompanying message and
// reviving the session if it has died. Moving to closed reclaims its resources before
// filing the card. Everything else is bookkeeping.
//
// This lives here rather than inside SetStatus so the lane rules stay free of
// subprocess work and remain testable on their own.
func (s *Server) dispatchStatus(
	ctx context.Context,
	id mission.MissionID,
	current mission.Mission,
	req api.SetStatusRequest,
) (mission.Mission, error) {
	if req.To == mission.StatusClosed {
		ms, _, err := s.svc.FinishMission(ctx, id, req.Force)

		return ms, err
	}

	if req.To != mission.StatusActive {
		return s.svc.SetStatus(id, req.To)
	}

	if !current.Launched() {
		return s.svc.Start(ctx, id)
	}

	switch current.Status {
	case mission.StatusBriefing:
		return s.svc.Start(ctx, id)
	case mission.StatusAwaiting, mission.StatusDebrief:
		return s.svc.Resume(ctx, id, req.Message)
	default:
		return s.svc.SetStatus(id, req.To)
	}
}

// handleSetStatus moves a mission between lanes.
func (s *Server) handleSetStatus(w http.ResponseWriter, r *http.Request) {
	var req api.SetStatusRequest
	if !decode(w, r, &req) {
		return
	}

	id := mission.MissionID(r.PathValue("id"))

	current, ok := s.svc.Snapshot().Mission(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("%w: mission %s", ErrNotFound, id))

		return
	}

	ms, err := s.dispatchStatus(r.Context(), id, current, req)
	if err != nil {
		writeServiceError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, ms)
}

// decode reads a JSON body, writing a 400 and returning false on failure.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decoding request body: %w", err))

		return false
	}

	return true
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	_ = json.NewEncoder(w).Encode(payload)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, api.Error{Error: err.Error()})
}

// writeServiceError maps a service sentinel error to an HTTP status.
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
