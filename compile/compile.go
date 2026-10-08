// Package compile runs the engine's compile command for linked accounts,
// one at a time (ADR 0001 sections 1 and 3). The source-specific work,
// including the source's API key, stays in that command: it reads the key
// from its own environment variable, which it inherits from this process.
package compile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yaad-index/taste-machine-telegram/userfiles"
)

// Timeout bounds one compile.
const Timeout = 10 * time.Minute

var sourceName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidateLink checks a link's source and user name before they reach the
// command line.
func ValidateLink(l userfiles.Link) error {
	if !sourceName.MatchString(l.Source) {
		return errors.New("a source is a lower-case name such as the one in /help")
	}
	if l.User == "" || len(l.User) > 64 || strings.ContainsAny(l.User, "\n\r\t") {
		return errors.New("a user name is one line of at most 64 characters")
	}
	return nil
}

// CLI runs `<Path> compile <source> --user=<name> --out=<dir>`.
type CLI struct {
	Path string
	// Env is the command's environment.
	Env []string
	// Secrets are values masked in any error text.
	Secrets []string
}

// Run compiles link into out. A failure is reported as one line: the
// command's last line of error output, with secrets masked.
func (c CLI) Run(ctx context.Context, link userfiles.Link, out string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, "compile", link.Source, "--user="+link.User, "--out="+out)
	cmd.Env = c.Env
	// Stopping kills the whole process group, so a child the command
	// started cannot hold its output open and keep Run waiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("the compile was stopped: %w", ctx.Err())
	}
	line := lastLine(stderr.String())
	if line == "" {
		line = err.Error()
	}
	for _, s := range c.Secrets {
		if s != "" {
			line = strings.ReplaceAll(line, s, "***")
		}
	}
	return errors.New(line)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Runner compiles a link into a directory.
type Runner interface {
	Run(ctx context.Context, link userfiles.Link, out string) error
}

// Job is one queued compile.
type Job struct {
	User int64
	Link userfiles.Link
	// Chat and Message are the "compiling…" reply to edit when done.
	Chat    int64
	Message int
}

// Done reports a finished job: the summary of the new files, or the error.
type Done func(ctx context.Context, j Job, sum userfiles.Summary, err error)

// Queue runs jobs one at a time, in order.
type Queue struct {
	run   Runner
	files *userfiles.Store
	keep  func(user int64) bool
	done  Done

	mu      sync.Mutex
	pending []Job
	busy    map[int64]bool
	wake    chan struct{}
}

// NewQueue returns a queue. keep is asked just before new files are
// committed, so a user revoked while compiling gets nothing written.
func NewQueue(run Runner, files *userfiles.Store, keep func(user int64) bool, done Done) *Queue {
	return &Queue{run: run, files: files, keep: keep, done: done, busy: map[int64]bool{}, wake: make(chan struct{}, 1)}
}

// Add queues j. It returns false, and queues nothing, when the user
// already has a compile queued or running.
func (q *Queue) Add(j Job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.busy[j.User] {
		return false
	}
	q.busy[j.User] = true
	q.pending = append(q.pending, j)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// Busy reports whether the user has a compile queued or running.
func (q *Queue) Busy(user int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.busy[user]
}

// Run works through the queue until ctx ends. A job cut short by ctx is
// still reported, with a context that outlives ctx briefly, so its reply
// can be edited.
func (q *Queue) Run(ctx context.Context) {
	for {
		j, ok := q.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
				continue
			}
		}
		sum, err := q.compile(ctx, j)
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		q.done(reportCtx, j, sum, err)
		cancel()
		q.mu.Lock()
		delete(q.busy, j.User)
		q.mu.Unlock()
		if ctx.Err() != nil {
			q.drop()
			return
		}
	}
}

func (q *Queue) next() (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Job{}, false
	}
	j := q.pending[0]
	q.pending = q.pending[1:]
	return j, true
}

// drop reports every job still queued as stopped.
func (q *Queue) drop() {
	for {
		j, ok := q.next()
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		q.done(ctx, j, userfiles.Summary{}, errors.New("the bot is stopping"))
		cancel()
		q.mu.Lock()
		delete(q.busy, j.User)
		q.mu.Unlock()
	}
}

func (q *Queue) compile(ctx context.Context, j Job) (userfiles.Summary, error) {
	staged, err := q.files.Stage(j.User)
	if err != nil {
		return userfiles.Summary{}, err
	}
	if err := q.run.Run(ctx, j.Link, staged); err != nil {
		q.files.Discard(staged)
		return userfiles.Summary{}, err
	}
	return q.files.Commit(j.User, staged, j.Link, func() bool { return q.keep(j.User) })
}
