package bot_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/schema"

	"github.com/yaad-index/taste-machine-telegram/access"
	"github.com/yaad-index/taste-machine-telegram/bot"
	"github.com/yaad-index/taste-machine-telegram/compile"
	"github.com/yaad-index/taste-machine-telegram/flow"
	"github.com/yaad-index/taste-machine-telegram/userfiles"
)

const (
	admin   = int64(1)
	member  = int64(7)
	group   = int64(-100)
	botName = "TestBot"
)

type sent struct {
	chat    int64
	text    string
	buttons [][]flow.Button
	// first is the text the message was sent with, before any edit.
	first string
}

// fakeSender records messages; an edit replaces the message's text in
// place. Message ids are indexes into msgs plus one.
type fakeSender struct {
	mu    sync.Mutex
	msgs  []sent
	edits int
	taps  []string
	fail  bool
}

func (f *fakeSender) Send(_ context.Context, chat int64, text string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, sent{chat: chat, text: text, first: text})
	if f.fail {
		return 0, errors.New("send failed")
	}
	return len(f.msgs), nil
}

func (f *fakeSender) Edit(_ context.Context, chat int64, id int, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id < 1 || id > len(f.msgs) || f.msgs[id-1].chat != chat {
		return errors.New("no such message")
	}
	f.msgs[id-1].text = text
	f.edits++
	return nil
}

func (f *fakeSender) SendScreen(_ context.Context, chat int64, s flow.Screen) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, sent{chat: chat, text: s.Text, buttons: s.Buttons, first: s.Text})
	return len(f.msgs), nil
}

func (f *fakeSender) EditScreen(_ context.Context, chat int64, id int, s flow.Screen) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id < 1 || id > len(f.msgs) || f.msgs[id-1].chat != chat {
		return errors.New("no such message")
	}
	f.msgs[id-1].text, f.msgs[id-1].buttons = s.Text, s.Buttons
	f.edits++
	return nil
}

func (f *fakeSender) AnswerTap(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taps = append(f.taps, id+": "+text)
	return nil
}

func (f *fakeSender) message(id int) sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msgs[id-1]
}

func (f *fakeSender) text(id int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msgs[id-1].text
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

// lastTo is the last message sent to chat after the first n messages, or
// "" when there is none.
func (f *fakeSender) lastTo(chat int64, n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.msgs) - 1; i >= n; i-- {
		if f.msgs[i].chat == chat {
			return f.msgs[i].text
		}
	}
	return ""
}

// fakeRunner writes a shelf of six items with two rated, after gate
// lets it through when gate is set, or fails for user names in fail.
type fakeRunner struct {
	gate chan struct{}
	// started gets a value when a gated compile reaches the gate.
	started chan struct{}
	fail    map[string]bool
}

func (r *fakeRunner) Run(ctx context.Context, l userfiles.Link, out string) error {
	if r.gate != nil {
		r.started <- struct{}{}
		select {
		case <-r.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.fail[l.User] {
		return errors.New("taste-machine: error: no such user")
	}
	sch := schema.Schema{Fields: []schema.Field{
		{Name: "theme", Type: schema.Category, Role: schema.Preference, Askable: true},
		{Name: "name", Type: schema.Category, Role: schema.Info, Display: true},
	}}
	shelf := &fileformat.Catalogue{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindCatalogue, Schema: sch}}
	taste := &fileformat.Taste{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindTaste}}
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		theme := []string{"sea", "sea", "space", "space", "forest", "forest"}[i]
		shelf.Items = append(shelf.Items, fileformat.Item{ID: id, Facts: map[string]schema.Value{
			"theme": {Type: schema.Category, Category: theme},
			"name":  {Type: schema.Category, Category: "Item " + strings.ToUpper(id)},
		}})
		it := fileformat.TasteItem{ID: id, Plays: 1}
		if i < 2 {
			rating := 7.0
			it.Rating = &rating
		}
		taste.Items = append(taste.Items, it)
	}
	return errors.Join(fileformat.WriteFile(filepath.Join(out, userfiles.ShelfFile), shelf), fileformat.WriteFile(filepath.Join(out, userfiles.TasteFile), taste))
}

// syncBuffer is a log sink the queue's goroutine and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	dir    string
	store  *access.Store
	files  *userfiles.Store
	send   *fakeSender
	runner *fakeRunner
	logs   *syncBuffer
	now    time.Time
	bot    *bot.Bot
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), send: &fakeSender{}, runner: &fakeRunner{}, logs: &syncBuffer{}, now: time.Date(2030, 1, 2, 10, 0, 0, 0, time.UTC)}
	var err error
	f.store, err = access.Open(filepath.Join(f.dir, "allowlist.json"), []int64{admin}, time.Now)
	require.NoError(t, err)
	f.files = userfiles.New(f.dir)
	var b *bot.Bot
	queue := compile.NewQueue(f.runner, f.files, f.store.UserAllowed, func(ctx context.Context, j compile.Job, sum userfiles.Summary, err error) {
		b.CompileDone(ctx, j, sum, err)
	})
	b = bot.New(bot.Deps{Access: f.store, Files: f.files, Queue: queue, Send: f.send, Name: botName, Log: slog.New(slog.NewTextHandler(f.logs, nil)), Now: func() time.Time { return f.now }})
	f.bot = b
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { queue.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return f
}

// say sends text in user's private chat and returns the reply there.
func (f *fixture) say(user int64, text string) string {
	n := f.send.count()
	f.bot.Handle(context.Background(), bot.Update{ChatID: user, Private: true, UserID: user, FirstName: "Someone", Text: text})
	return f.send.lastTo(user, n)
}

// sayInGroup sends text in the group chat and returns the reply there.
func (f *fixture) sayInGroup(user int64, text string) string {
	n := f.send.count()
	f.bot.Handle(context.Background(), bot.Update{ChatID: group, UserID: user, FirstName: "Someone", Text: text})
	return f.send.lastTo(group, n)
}

// admit invites user through an admin's link.
func (f *fixture) admit(t *testing.T, user int64) {
	t.Helper()
	token := regexp.MustCompile(`start=(\S+)`).FindStringSubmatch(f.say(admin, "/invite"))
	require.Len(t, token, 2)
	require.Equal(t, bot.MsgWelcome, f.say(user, "/start "+token[1]))
}

func TestStrangerIsRefusedAndNothingIsKept(t *testing.T) {
	f := newFixture(t)
	for _, text := range []string{"hello", "/help", "/start", "/invite", "/start bogus-token"} {
		reply := f.say(42, text)
		if text == "/start bogus-token" {
			assert.Equal(t, bot.MsgInviteInvalid, reply)
		} else {
			assert.Equal(t, bot.MsgRefused, reply, text)
		}
	}
	assert.Equal(t, bot.MsgRefused, f.sayInGroup(42, "/help"))
	assert.Empty(t, f.sayInGroup(42, "just talking"), "plain group messages get no answer")
	assert.False(t, f.store.UserAllowed(42))
	assert.Empty(t, f.logs.String(), "nothing about a stranger is logged")
	_, err := os.Stat(filepath.Join(f.dir, "allowlist.json"))
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing about a stranger is stored")
}

func TestInvite(t *testing.T) {
	f := newFixture(t)
	reply := f.say(admin, "/invite")
	assert.Regexp(t, `^One-time invite, valid until \d{4}-\d\d-\d\d \d\d:\d\d UTC:\nhttps://t\.me/TestBot\?start=[A-Za-z0-9_-]{22}$`, reply)
	token := regexp.MustCompile(`start=(\S+)`).FindStringSubmatch(reply)[1]

	f.bot.Handle(context.Background(), bot.Update{ChatID: member, Private: true, UserID: member, FirstName: "Newcomer", Text: "/start " + token})
	n := f.send.count()
	require.GreaterOrEqual(t, n, 2)
	assert.Equal(t, sent{chat: member, text: bot.MsgWelcome, first: bot.MsgWelcome}, f.send.msgs[n-2])
	assert.Equal(t, "Newcomer joined through an invite (user id 7). /revoke 7 removes them.", f.send.msgs[n-1].text)
	assert.Equal(t, admin, f.send.msgs[n-1].chat)
	assert.True(t, f.store.UserAllowed(member))

	assert.Equal(t, bot.MsgInviteInvalid, f.say(43, "/start "+token), "a link admits one person")
	assert.False(t, f.store.UserAllowed(43))
	assert.NotContains(t, f.logs.String(), token)

	assert.NotContains(t, f.say(member, "/start "+token), "invite", "an allowed user's /start shows help and spends nothing")
}

func TestAdminCommandsNeedAnAdmin(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	assert.Equal(t, bot.MsgAdminOnly, f.say(member, "/invite"))
	assert.Equal(t, bot.MsgAdminOnly, f.say(member, "/revoke 1"))
	assert.NotContains(t, f.say(member, "/help"), "/invite", "help shows admin commands to admins only")
	assert.Contains(t, f.say(admin, "/help"), "/invite")
}

func TestRevoke(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	dir := f.files.Dir(member)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "taste.zip"), []byte("x"), 0o600))

	assert.Equal(t, "User 7 is revoked and their files are deleted.", f.say(admin, "/revoke 7"))
	assert.False(t, f.store.UserAllowed(member))
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, bot.MsgRefused, f.say(member, "/help"))

	assert.Equal(t, "User 99 was not on the allowlist; any files they had are deleted.", f.say(admin, "/revoke 99"))
	assert.Equal(t, bot.MsgRevokeUsage, f.say(admin, "/revoke"))
	assert.Equal(t, bot.MsgRevokeUsage, f.say(admin, "/revoke seven"))
	assert.Equal(t, "Admins come from the configuration and cannot be revoked.", f.say(admin, "/revoke 1"))
}

func TestGroupChats(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	assert.Equal(t, bot.MsgChatNotAllowed, f.sayInGroup(member, "/help"))
	assert.Equal(t, bot.MsgAdminOnly, f.sayInGroup(member, "/allow"))
	assert.False(t, f.store.ChatAllowed(group))

	assert.Equal(t, bot.MsgChatAllowed, f.sayInGroup(admin, "/allow@testbot"), "the bot's name matches in any case")
	assert.True(t, f.store.ChatAllowed(group))
	assert.Contains(t, f.sayInGroup(member, "/help"), "Commands:")
	assert.Empty(t, f.sayInGroup(member, "/help@OtherBot"), "a command for another bot is ignored")
	assert.Equal(t, bot.MsgInPrivateOnly, f.sayInGroup(admin, "/invite"))
	assert.Equal(t, bot.MsgInPrivateOnly, f.sayInGroup(admin, "/revoke 7"))
	assert.Equal(t, bot.MsgUnknown, f.sayInGroup(member, "/nope"))

	assert.Equal(t, bot.MsgChatDisallowed, f.sayInGroup(admin, "/disallow"))
	assert.False(t, f.store.ChatAllowed(group))
	assert.Equal(t, bot.MsgChatNotAllowed, f.sayInGroup(member, "/help"))
}

func TestPrivateChatRouting(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	assert.Equal(t, bot.MsgInGroupOnly, f.say(admin, "/allow"))
	assert.Equal(t, bot.MsgUnknown, f.say(member, "/nope"))
	assert.Contains(t, f.say(member, "hello"), "Commands:", "plain text gets the help")
	assert.Contains(t, f.say(member, "/HELP"), "Commands:", "commands are case-insensitive")
}

func TestStoreFailureIsReported(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, os.RemoveAll(f.dir))
	assert.Equal(t, bot.MsgSomethingFailed, f.sayInGroup(admin, "/allow"))
	assert.Equal(t, bot.MsgSomethingFailed, f.say(admin, "/invite"))
	assert.Contains(t, f.logs.String(), "changing the chat allowlist")
}

// release lets the gated runner finish its compile.
func (f *fixture) release(t *testing.T) {
	t.Helper()
	select {
	case f.runner.gate <- struct{}{}:
	case <-time.After(10 * time.Second):
		t.Fatal("no compile is waiting at the gate")
	}
}

// compiling sends text and returns the id of the "compiling…" reply.
// The queue may already have edited the reply by the time this looks at
// it, so it checks the text the reply was sent with.
func (f *fixture) compiling(t *testing.T, user int64, text string) int {
	t.Helper()
	n := f.send.count()
	f.say(user, text)
	f.send.mu.Lock()
	defer f.send.mu.Unlock()
	require.Greater(t, len(f.send.msgs), n, "no reply")
	reply := f.send.msgs[n]
	require.Equal(t, sent{chat: user, first: bot.MsgCompiling}, sent{chat: reply.chat, first: reply.first})
	return n + 1
}

// settled waits until message id no longer reads "compiling…".
func (f *fixture) settled(t *testing.T, id int) string {
	t.Helper()
	require.Eventually(t, func() bool { return f.send.text(id) != bot.MsgCompiling }, 10*time.Second, time.Millisecond)
	return f.send.text(id)
}

func TestLink(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	id := f.compiling(t, member, "/link SRC some one")
	assert.Equal(t, "Done: 6 items on your shelf, 2 of your items rated.", f.settled(t, id))
	l, err := f.files.Link(member)
	require.NoError(t, err)
	assert.Equal(t, userfiles.Link{Source: "src", User: "some one"}, l, "the source is lower-cased, the user name kept whole")
	assert.Contains(t, f.say(member, "/help"), "/link <source> <user name>")
}

func TestLinkUsage(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	for _, text := range []string{"/link", "/link src", "/link -x name", "/link src " + strings.Repeat("u", 65)} {
		assert.True(t, strings.HasPrefix(f.say(member, text), bot.MsgLinkUsage), text)
	}
	require.Equal(t, bot.MsgChatAllowed, f.sayInGroup(admin, "/allow"))
	for _, cmd := range []string{"/link src name", "/refresh", "/unlink"} {
		assert.Equal(t, bot.MsgInPrivateOnly, f.sayInGroup(admin, cmd), cmd)
	}
}

func TestCompileLimit(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	f.settled(t, f.compiling(t, member, "/link src a"))
	last, ok := f.files.LastCompile(member)
	require.True(t, ok)
	f.now = last.Add(bot.CompileEvery - time.Minute)
	want := "You can compile again after " + last.Add(bot.CompileEvery).UTC().Format("15:04 UTC") + "."
	assert.Equal(t, want, f.say(member, "/refresh"))
	assert.Equal(t, want, f.say(member, "/link src b"), "/link and /refresh share the limit")

	f.now = last.Add(bot.CompileEvery)
	assert.Contains(t, f.settled(t, f.compiling(t, member, "/refresh")), "Done:", "allowed again after an hour")
	l, err := f.files.Link(member)
	require.NoError(t, err)
	assert.Equal(t, "a", l.User, "/refresh recompiles the linked account")
}

func TestFailedCompileDoesNotUseTheHour(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	f.runner.fail = map[string]bool{"nobody": true}
	assert.Equal(t, "The compile failed: taste-machine: error: no such user", f.settled(t, f.compiling(t, member, "/link src nobody")))
	assert.Equal(t, bot.MsgNotLinked, f.say(member, "/refresh"))
	assert.Contains(t, f.settled(t, f.compiling(t, member, "/link src someone")), "Done:", "no hour was used")
}

func TestOneCompileAtATimePerUser(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	f.runner.gate, f.runner.started = make(chan struct{}), make(chan struct{}, 1)
	id := f.compiling(t, member, "/link src a")
	f.send.mu.Lock()
	edits := f.send.edits
	f.send.mu.Unlock()
	assert.Equal(t, bot.MsgAlreadyBusy, f.say(member, "/link src b"))
	f.send.mu.Lock()
	assert.Equal(t, edits, f.send.edits, "refused straight away, not by editing a \"compiling…\" reply")
	f.send.mu.Unlock()
	assert.Equal(t, bot.MsgAlreadyBusy, f.say(member, "/unlink"), "no unlink under a running compile")
	f.release(t)
	assert.Contains(t, f.settled(t, id), "Done:")
}

func TestUnlink(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	assert.Equal(t, bot.MsgNothingToUnlink, f.say(member, "/unlink"))
	f.settled(t, f.compiling(t, member, "/link src a"))
	assert.Equal(t, bot.MsgUnlinked, f.say(member, "/unlink"))
	assert.NoDirExists(t, f.files.Dir(member))
	assert.Equal(t, bot.MsgNotLinked, f.say(member, "/refresh"))
	assert.Contains(t, f.settled(t, f.compiling(t, member, "/link src a")), "Done:", "unlinking and linking again skips the limit (ADR 0001 section 3)")
}

func TestRevokeDuringACompileWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	f.runner.gate, f.runner.started = make(chan struct{}), make(chan struct{}, 1)
	id := f.compiling(t, member, "/link src a")
	select {
	case <-f.runner.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the compile did not start")
	}
	assert.Equal(t, "User 7 is revoked and their files are deleted.", f.say(admin, "/revoke 7"))
	f.release(t)
	assert.Contains(t, f.settled(t, id), "The compile failed")
	_, _, err := f.files.Paths(member)
	require.ErrorIs(t, err, userfiles.ErrNotLinked)
}

// press taps the button whose text starts with label on message id, as
// user in their private chat, and returns the tap's notice.
func (f *fixture) press(t *testing.T, user int64, id int, label string) string {
	t.Helper()
	for _, row := range f.send.message(id).buttons {
		for _, b := range row {
			if strings.HasPrefix(b.Text, label) {
				return f.pressData(user, id, b.Data)
			}
		}
	}
	t.Fatalf("no button %q on message %d: %q", label, id, f.send.message(id).text)
	return ""
}

func (f *fixture) pressData(user int64, id int, data string) string {
	f.send.mu.Lock()
	n := len(f.send.taps)
	f.send.mu.Unlock()
	f.bot.Handle(context.Background(), bot.Update{ChatID: user, Private: true, UserID: user, CallbackID: "cb", Data: data, MessageID: id})
	f.send.mu.Lock()
	defer f.send.mu.Unlock()
	if len(f.send.taps) == n {
		return "(no answer)"
	}
	return strings.TrimPrefix(f.send.taps[len(f.send.taps)-1], "cb: ")
}

// linked admits member and links them, so /pick has files to read.
func (f *fixture) linked(t *testing.T) {
	t.Helper()
	f.admit(t, member)
	f.settled(t, f.compiling(t, member, "/link src a"))
}

func TestPickNeedsALink(t *testing.T) {
	f := newFixture(t)
	f.admit(t, member)
	assert.Equal(t, bot.MsgNotLinked, f.say(member, "/pick"))
	assert.Contains(t, f.say(member, "/help"), "/pick")
}

func TestPickFlow(t *testing.T) {
	f := newFixture(t)
	f.linked(t)
	assert.Equal(t, "theme? (6 left)", f.say(member, "/pick"))
	q := f.send.count()
	assert.Len(t, f.send.message(q).buttons, 5, "three options, other and no preference, show results now")

	assert.Empty(t, f.press(t, member, q, "space"), "a tap is acknowledged without a notice")
	assert.Equal(t, sent{chat: member, text: "theme: space", first: "theme? (6 left)"}, f.send.message(q), "the question is edited in place to show the answer, without buttons")
	results := f.send.count()
	require.Equal(t, results, q+1)
	got := f.send.message(results)
	assert.True(t, strings.HasPrefix(got.text, "Top picks:\n1. "), got.text)
	assert.Contains(t, got.text, "Item C", "results carry display names")

	f.press(t, member, results, "why 1")
	assert.Equal(t, results+1, f.send.count())
	assert.Contains(t, f.send.message(results+1).text, "score ", "the full explanation")
}

func TestPickTapRules(t *testing.T) {
	f := newFixture(t)
	f.linked(t)
	f.say(member, "/pick")
	first := f.send.count()
	oldData := f.send.message(first).buttons[0][0].Data
	f.press(t, member, first, "no preference")
	assert.Equal(t, bot.MsgStaleTap, f.pressData(member, first, oldData))

	f.say(member, "/pick")
	assert.Equal(t, bot.MsgSessionEnded, f.pressData(member, first, oldData), "a new /pick replaces the session")

	second := f.send.count()
	f.now = f.now.Add(bot.SessionTTL)
	assert.Equal(t, bot.MsgSessionEnded, f.press(t, member, second, "space"), "sessions end after SessionTTL")

	assert.Equal(t, bot.MsgSessionEnded, f.pressData(member, second, "zzzz 1 o0"))
	f.now = f.now.Add(-bot.SessionTTL)
	f.say(member, "/pick")
	live := f.send.count()
	f.bot.Handle(context.Background(), bot.Update{ChatID: group, UserID: member, CallbackID: "cb", Data: f.send.message(live).buttons[0][0].Data, MessageID: live})
	f.send.mu.Lock()
	assert.Equal(t, "cb: "+bot.MsgSessionEnded, f.send.taps[len(f.send.taps)-1], "a private session's button does not work from a group chat")
	f.send.mu.Unlock()
	assert.Equal(t, bot.MsgRefused, f.pressData(42, second, "zzzz 1 o0"), "a stranger's tap is refused")
	assert.NotContains(t, f.logs.String(), "42")
}

func TestPickSessionsEndOnRestart(t *testing.T) {
	f := newFixture(t)
	f.linked(t)
	f.say(member, "/pick")
	q := f.send.count()
	// A new bot over the same allowlist and files, as after a restart.
	f.bot = bot.New(bot.Deps{Access: f.store, Files: f.files, Send: f.send, Name: botName, Log: slog.New(slog.NewTextHandler(f.logs, nil)), Now: func() time.Time { return f.now }})
	assert.Equal(t, bot.MsgSessionEnded, f.pressData(member, q, f.send.message(q).buttons[0][0].Data), "a restart forgets sessions")
}
