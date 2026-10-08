package health_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/taste-machine-telegram/health"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type status int

func (s status) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(s), Body: io.NopCloser(strings.NewReader(""))}, nil
}

func call(t *testing.T, d health.Doer, path string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://api"+path, nil)
	resp, err := d.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
}

func TestStatus(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	m := &health.Monitor{MaxAge: 3 * time.Minute, Now: c.now}
	ok, why := m.Status()
	assert.False(t, ok)
	assert.Equal(t, "the update loop is not running", why)

	m.Running(true)
	ok, _ = m.Status()
	assert.True(t, ok, "starting counts as a fresh poll")

	c.t = c.t.Add(3 * time.Minute)
	ok, _ = m.Status()
	assert.True(t, ok, "healthy up to MaxAge")
	c.t = c.t.Add(time.Second)
	ok, why = m.Status()
	assert.False(t, ok)
	assert.Equal(t, "no successful poll since 1970-01-01T00:16:40Z", why)

	call(t, m.Wrap(status(http.StatusOK)), "/bot123:x/getUpdates")
	ok, _ = m.Status()
	assert.True(t, ok, "a successful poll makes it healthy again")

	m.Running(false)
	ok, _ = m.Status()
	assert.False(t, ok)
}

func TestOnlySuccessfulPollsCount(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	m := &health.Monitor{MaxAge: time.Minute, Now: c.now}
	m.Running(true)
	c.t = c.t.Add(2 * time.Minute)
	call(t, m.Wrap(status(http.StatusUnauthorized)), "/bot123:x/getUpdates")
	call(t, m.Wrap(status(http.StatusOK)), "/bot123:x/sendMessage")
	ok, _ := m.Status()
	assert.False(t, ok, "a failed poll or another method does not count")
}

func TestEndpointAndCheck(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	m := &health.Monitor{MaxAge: time.Minute, Now: c.now}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle(health.Path, m)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	addr := ln.Addr().String()

	err = health.Check(context.Background(), addr)
	require.EqualError(t, err, "unhealthy (503)")
	m.Running(true)
	require.NoError(t, health.Check(context.Background(), addr))
	_, port, _ := strings.Cut(addr, ":")
	require.NoError(t, health.Check(context.Background(), ":"+port), "a bare :port means this host")

	resp, err := http.Get("http://" + addr + health.Path)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, "ok\n", string(body))
}
