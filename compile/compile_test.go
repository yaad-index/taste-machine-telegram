package compile_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/schema"

	"github.com/yaad-index/taste-machine-telegram/compile"
	"github.com/yaad-index/taste-machine-telegram/userfiles"
)

func TestValidateLink(t *testing.T) {
	require.NoError(t, compile.ValidateLink(userfiles.Link{Source: "src", User: "some one"}))
	require.NoError(t, compile.ValidateLink(userfiles.Link{Source: "a-1", User: "--looks-like-a-flag"}), "the name is passed as --user=NAME")
	for _, l := range []userfiles.Link{
		{Source: "", User: "u"},
		{Source: "Src", User: "u"},
		{Source: "-x", User: "u"},
		{Source: "src", User: ""},
		{Source: "src", User: "two\nlines"},
		{Source: "src", User: strings.Repeat("u", 65)},
	} {
		assert.Error(t, compile.ValidateLink(l), "%+v", l)
	}
}

// fakeCommand writes a shell script that records its arguments and
// environment, and fails when FAIL is set.
func fakeCommand(t *testing.T) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "taste-machine")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$OUT_DIR/args"
env > "$OUT_DIR/env"
if [ -n "$SLEEP" ]; then sleep "$SLEEP"; fi
if [ -n "$FAIL" ]; then
  echo "note: something on the way" >&2
  echo "taste-machine: error: the source refused key $SOURCE_KEY" >&2
  exit 1
fi
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
	return path, dir
}

func TestCLIRun(t *testing.T) {
	path, dir := fakeCommand(t)
	cli := compile.CLI{Path: path, Env: []string{"OUT_DIR=" + dir, "SOURCE_KEY=k-123", "PATH=" + os.Getenv("PATH")}}
	require.NoError(t, cli.Run(context.Background(), userfiles.Link{Source: "src", User: "some one"}, "/out/v-1"))
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	require.NoError(t, err)
	assert.Equal(t, "compile\nsrc\n--user=some one\n--out=/out/v-1\n", string(args))
	env, err := os.ReadFile(filepath.Join(dir, "env"))
	require.NoError(t, err)
	assert.Contains(t, string(env), "SOURCE_KEY=k-123", "the command gets the environment it is given")
}

func TestCLIRunReportsOneMaskedLine(t *testing.T) {
	path, dir := fakeCommand(t)
	cli := compile.CLI{Path: path, Env: []string{"OUT_DIR=" + dir, "FAIL=1", "SOURCE_KEY=k-123", "PATH=" + os.Getenv("PATH")}, Secrets: []string{"k-123", ""}}
	err := cli.Run(context.Background(), userfiles.Link{Source: "src", User: "u"}, t.TempDir())
	require.EqualError(t, err, "taste-machine: error: the source refused key ***")
}

func TestCLIRunStops(t *testing.T) {
	path, dir := fakeCommand(t)
	cli := compile.CLI{Path: path, Env: []string{"OUT_DIR=" + dir, "SLEEP=5", "PATH=" + os.Getenv("PATH")}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := cli.Run(ctx, userfiles.Link{Source: "src", User: "u"}, t.TempDir())
	require.ErrorContains(t, err, "the compile was stopped")
	assert.Less(t, time.Since(start), 4*time.Second)
}

func TestCLIRunMissingCommand(t *testing.T) {
	err := compile.CLI{Path: filepath.Join(t.TempDir(), "nope")}.Run(context.Background(), userfiles.Link{Source: "src", User: "u"}, t.TempDir())
	require.Error(t, err)
}

// fakeRunner writes a valid pair, or fails, and records the order and how
// many runs overlapped.
type fakeRunner struct {
	mu      sync.Mutex
	order   []string
	running int
	maxRun  int
	fail    map[string]bool
	gate    chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context, l userfiles.Link, out string) error {
	f.mu.Lock()
	f.order = append(f.order, l.User)
	f.running++
	f.maxRun = max(f.maxRun, f.running)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	time.Sleep(5 * time.Millisecond)
	if f.fail[l.User] {
		return errors.New("compile failed")
	}
	return writePair(out, 3, 2)
}

func writePair(dir string, n, rated int) error {
	s := schema.Schema{Fields: []schema.Field{{Name: "theme", Type: schema.Category, Role: schema.Preference}}}
	shelf := &fileformat.Catalogue{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindCatalogue, Schema: s}}
	taste := &fileformat.Taste{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindTaste}}
	for i := range n {
		id := fmt.Sprintf("i%d", i)
		shelf.Items = append(shelf.Items, fileformat.Item{ID: id, Facts: map[string]schema.Value{"theme": {Type: schema.Category, Category: "sea"}}})
		it := fileformat.TasteItem{ID: id, Plays: 1}
		if i < rated {
			r := 5.0
			it.Rating = &r
		}
		taste.Items = append(taste.Items, it)
	}
	return errors.Join(fileformat.WriteFile(filepath.Join(dir, userfiles.ShelfFile), shelf), fileformat.WriteFile(filepath.Join(dir, userfiles.TasteFile), taste))
}

type result struct {
	job compile.Job
	sum userfiles.Summary
	err error
}

type harness struct {
	files   *userfiles.Store
	runner  *fakeRunner
	queue   *compile.Queue
	results chan result
	revoked sync.Map
}

func newHarness(t *testing.T, runner *fakeRunner) *harness {
	t.Helper()
	h := &harness{files: userfiles.New(t.TempDir()), runner: runner, results: make(chan result, 16)}
	h.queue = compile.NewQueue(runner, h.files,
		func(user int64) bool { _, gone := h.revoked.Load(user); return !gone },
		func(_ context.Context, j compile.Job, sum userfiles.Summary, err error) {
			h.results <- result{j, sum, err}
		})
	return h
}

func (h *harness) wait(t *testing.T) result {
	t.Helper()
	select {
	case r := <-h.results:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no result")
		return result{}
	}
}

func job(user int64, name string) compile.Job {
	return compile.Job{User: user, Link: userfiles.Link{Source: "src", User: name}, Chat: user, Message: 1}
}

func TestQueueRunsOneAtATimeInOrder(t *testing.T) {
	h := newHarness(t, &fakeRunner{})
	for i, name := range []string{"a", "b", "c"} {
		require.True(t, h.queue.Add(job(int64(i+1), name)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.queue.Run(ctx)
	for i := range 3 {
		r := h.wait(t)
		require.NoError(t, r.err)
		assert.Equal(t, int64(i+1), r.job.User)
		assert.Equal(t, userfiles.Summary{Shelf: 3, Rated: 2}, r.sum)
		l, err := h.files.Link(int64(i + 1))
		require.NoError(t, err)
		assert.Equal(t, r.job.Link, l)
	}
	assert.Equal(t, []string{"a", "b", "c"}, h.runner.order)
	assert.Equal(t, 1, h.runner.maxRun, "one compile at a time")
}

func TestQueueOneJobPerUser(t *testing.T) {
	h := newHarness(t, &fakeRunner{gate: make(chan struct{})})
	require.True(t, h.queue.Add(job(7, "a")))
	assert.False(t, h.queue.Add(job(7, "b")), "a second compile for the same user is refused")
	assert.True(t, h.queue.Busy(7))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.queue.Run(ctx)
	h.runner.gate <- struct{}{}
	require.NoError(t, h.wait(t).err)
	assert.Eventually(t, func() bool { return !h.queue.Busy(7) }, time.Second, time.Millisecond)
	assert.True(t, h.queue.Add(job(7, "b")), "free again once done")
}

func TestQueueFailureWritesNothing(t *testing.T) {
	h := newHarness(t, &fakeRunner{fail: map[string]bool{"a": true}})
	require.True(t, h.queue.Add(job(7, "a")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.queue.Run(ctx)
	r := h.wait(t)
	require.EqualError(t, r.err, "compile failed")
	_, _, err := h.files.Paths(7)
	require.ErrorIs(t, err, userfiles.ErrNotLinked)
	_, ok := h.files.LastCompile(7)
	assert.False(t, ok, "a failed compile does not use up the hour")
	staged, _ := filepath.Glob(filepath.Join(h.files.Dir(7), "v-*"))
	assert.Empty(t, staged, "the staged directory is removed")
}

func TestQueueDiscardsForARevokedUser(t *testing.T) {
	h := newHarness(t, &fakeRunner{})
	h.revoked.Store(int64(7), true)
	require.True(t, h.queue.Add(job(7, "a")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.queue.Run(ctx)
	require.Error(t, h.wait(t).err)
	_, _, err := h.files.Paths(7)
	require.ErrorIs(t, err, userfiles.ErrNotLinked)
}

func TestQueueStopReportsEveryJob(t *testing.T) {
	h := newHarness(t, &fakeRunner{gate: make(chan struct{})})
	require.True(t, h.queue.Add(job(1, "a")))
	require.True(t, h.queue.Add(job(2, "b")))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { h.queue.Run(ctx); close(stopped) }()
	assert.Eventually(t, func() bool { h.runner.mu.Lock(); defer h.runner.mu.Unlock(); return h.runner.running == 1 }, time.Second, time.Millisecond)
	cancel()
	<-stopped
	r1, r2 := h.wait(t), h.wait(t)
	assert.Equal(t, int64(1), r1.job.User)
	require.ErrorIs(t, r1.err, context.Canceled)
	assert.Equal(t, int64(2), r2.job.User)
	require.EqualError(t, r2.err, "the bot is stopping")
	assert.False(t, h.queue.Busy(1))
	assert.False(t, h.queue.Busy(2))
}
