package codexapp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/justinrush/q/internal/runner"
)

const initializeTimeout = 5 * time.Second

const daemonStartTimeout = 10 * time.Second

// Proxy is a Q observer connection to Codex's managed app-server.
// It intentionally reads threads without resuming them, so the terminal UI
// remains the sole interactive subscriber and owner of approval requests.
type Proxy struct {
	client  *Client
	process *runner.Stream
}

// ThreadSnapshot identifies one thread and its current runtime status.
type ThreadSnapshot struct {
	ID     string       `json:"id"`
	Status ThreadStatus `json:"status"`
}

// StartProxy connects to the managed app-server through Codex's stdio proxy.
func StartProxy(ctx context.Context, codexBin, version string, run runner.OS) (*Proxy, error) {
	process, err := run.StartStream(ctx, runner.Spec{
		Name: codexBin,
		Args: []string{"app-server", "proxy"},
	})
	if err != nil {
		return nil, err
	}

	client := NewClient(process.Stdout, process.Stdin)
	notifications := make(chan Notification, 64)

	go func() {
		for range notifications {
		}
	}()

	go func() {
		_ = client.Run(ctx, notifications)
		close(notifications)
	}()

	initCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()

	err = client.Initialize(initCtx, "q", "Q", version)
	if err != nil {
		_ = process.Stop()

		return nil, fmt.Errorf("initializing Codex app-server proxy: %w", err)
	}

	go func() { _ = process.Wait() }()

	return &Proxy{client: client, process: process}, nil
}

// ReadThread returns the current runtime status without subscribing this
// connection to the thread's interactive event stream.
func (p *Proxy) ReadThread(ctx context.Context, threadID string) (ThreadStatus, error) {
	params := struct {
		ThreadID     string `json:"threadId"`
		IncludeTurns bool   `json:"includeTurns"`
	}{
		ThreadID: threadID,
	}

	var result struct {
		Thread struct {
			ID     string       `json:"id"`
			Status ThreadStatus `json:"status"`
		} `json:"thread"`
	}

	err := p.client.Request(ctx, "thread/read", params, &result)
	if err != nil {
		return ThreadStatus{}, err
	}

	if result.Thread.ID == "" || result.Thread.Status.Type == "" {
		return ThreadStatus{}, fmt.Errorf("thread/read returned no thread status")
	}

	return result.Thread.Status, nil
}

// FindThread locates the loaded thread for an exact working directory. Task
// directories are unique, so this recovers a session id even when SessionStart
// hooks were missed.
func (p *Proxy) FindThread(ctx context.Context, cwd string) (ThreadSnapshot, bool, error) {
	params := struct {
		CWD   string `json:"cwd"`
		Limit int    `json:"limit"`
	}{
		CWD:   cwd,
		Limit: 20,
	}

	var result struct {
		Data []ThreadSnapshot `json:"data"`
	}

	err := p.client.Request(ctx, "thread/list", params, &result)
	if err != nil {
		return ThreadSnapshot{}, false, err
	}

	for _, thread := range result.Data {
		if thread.ID != "" && thread.Status.Classify() != ActivityUnknown {
			return thread, true, nil
		}
	}

	return ThreadSnapshot{}, false, nil
}

// Close stops the local proxy process. It does not stop the managed app-server.
func (p *Proxy) Close() error {
	return p.process.Stop()
}

// Manager starts and connects to the managed app-server on first use. Keeping
// startup lazy means running Q has no Codex side effect until a Codex task
// actually has a session to observe.
type Manager struct {
	ctx      context.Context
	codexBin string
	version  string
	run      runner.OS

	mu    sync.Mutex
	proxy *Proxy
}

// NewManager returns a lazy managed app-server connection.
func NewManager(ctx context.Context, codexBin, version string, run runner.OS) *Manager {
	return &Manager{ctx: ctx, codexBin: codexBin, version: version, run: run}
}

// ReadThread ensures the managed server is running, then reads one thread.
func (m *Manager) ReadThread(ctx context.Context, threadID string) (ThreadStatus, error) {
	var status ThreadStatus
	err := m.withProxy(func(proxy *Proxy) error {
		var readErr error
		status, readErr = proxy.ReadThread(ctx, threadID)

		return readErr
	})
	if err != nil {
		return ThreadStatus{}, err
	}

	return status, nil
}

// FindThread locates a loaded thread by its exact working directory.
func (m *Manager) FindThread(ctx context.Context, cwd string) (ThreadSnapshot, bool, error) {
	var (
		thread ThreadSnapshot
		found  bool
	)

	err := m.withProxy(func(proxy *Proxy) error {
		var readErr error
		thread, found, readErr = proxy.FindThread(ctx, cwd)

		return readErr
	})
	if err != nil {
		return ThreadSnapshot{}, false, err
	}

	return thread, found, nil
}

// withProxy retries once with a fresh proxy. A Q daemon can outlive a
// Codex app-server restart, and holding a dead pipe forever would silently
// return the integration to stale hook-only behavior.
func (m *Manager) withProxy(call func(*Proxy) error) error {
	proxy, err := m.connect()
	if err != nil {
		return err
	}

	err = call(proxy)
	if err == nil {
		return nil
	}

	m.discard(proxy)

	proxy, reconnectErr := m.connect()
	if reconnectErr != nil {
		return fmt.Errorf("codex app-server request failed (%v), reconnecting failed: %w", err, reconnectErr)
	}

	return call(proxy)
}

func (m *Manager) connect() (*Proxy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.proxy != nil {
		return m.proxy, nil
	}

	startCtx, cancel := context.WithTimeout(m.ctx, daemonStartTimeout)
	defer cancel()

	_, err := m.run.Run(startCtx, runner.Spec{
		Name: m.codexBin,
		Args: []string{"app-server", "daemon", "start"},
	})
	if err != nil {
		return nil, fmt.Errorf("starting Codex app-server daemon: %w", err)
	}

	proxy, err := StartProxy(m.ctx, m.codexBin, m.version, m.run)
	if err != nil {
		return nil, err
	}

	m.proxy = proxy

	return proxy, nil
}

// Close stops the observer proxy when it was started.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.proxy == nil {
		return nil
	}

	err := m.proxy.Close()
	m.proxy = nil

	return err
}

func (m *Manager) discard(proxy *Proxy) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.proxy != proxy {
		return
	}

	_ = m.proxy.Close()
	m.proxy = nil
}
