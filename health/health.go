// Package health tells monitoring whether the bot is working (ADR 0001
// section 6): healthy while the update loop runs and its long polls keep
// succeeding. An idle bot still polls, so a stale last poll means the bot
// cannot reach the Bot API, whatever the reason.
package health

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Path is where the endpoint answers.
const Path = "/healthz"

// Monitor records the update loop's state and its last successful poll.
type Monitor struct {
	// MaxAge is how old the last successful poll may be.
	MaxAge time.Duration
	Now    func() time.Time

	mu       sync.Mutex
	running  bool
	lastPoll time.Time
}

// Running marks the update loop as started or stopped. Starting counts as
// a fresh poll, so the first long poll has MaxAge to come back.
func (m *Monitor) Running(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = on
	if on {
		m.lastPoll = m.Now()
	}
}

// Status reports whether the bot is healthy, and why not.
func (m *Monitor) Status() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case !m.running:
		return false, "the update loop is not running"
	case m.Now().Sub(m.lastPoll) > m.MaxAge:
		return false, fmt.Sprintf("no successful poll since %s", m.lastPoll.UTC().Format(time.RFC3339))
	}
	return true, "ok"
}

// ServeHTTP answers 200 when healthy and 503 otherwise.
func (m *Monitor) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	ok, why := m.Status()
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = fmt.Fprintln(w, why)
}

// Doer is the Telegram client's HTTP client.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Wrap returns a client that records every successful getUpdates call.
func (m *Monitor) Wrap(inner Doer) Doer { return polls{inner: inner, m: m} }

type polls struct {
	inner Doer
	m     *Monitor
}

func (p polls) Do(r *http.Request) (*http.Response, error) {
	resp, err := p.inner.Do(r)
	if err == nil && resp.StatusCode == http.StatusOK && strings.HasSuffix(r.URL.Path, "/getUpdates") {
		p.m.mu.Lock()
		p.m.lastPoll = p.m.Now()
		p.m.mu.Unlock()
	}
	return resp, err
}

// Check asks the endpoint at addr, for a container's health check. It
// returns nil when healthy. An address with no host, such as ":8080",
// reaches this machine.
func Check(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+Path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy (%d)", resp.StatusCode)
	}
	return nil
}
