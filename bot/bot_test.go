package bot_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/taste-machine-telegram/access"
	"github.com/yaad-index/taste-machine-telegram/bot"
)

const (
	admin   = int64(1)
	member  = int64(7)
	group   = int64(-100)
	botName = "TestBot"
)

type sent struct {
	chat int64
	text string
}

type fakeSender struct {
	msgs []sent
	fail bool
}

func (f *fakeSender) Send(_ context.Context, chat int64, text string) error {
	f.msgs = append(f.msgs, sent{chat, text})
	if f.fail {
		return errors.New("send failed")
	}
	return nil
}

// lastTo is the last message sent to chat after the first n messages, or
// "" when there is none.
func (f *fakeSender) lastTo(chat int64, n int) string {
	for i := len(f.msgs) - 1; i >= n; i-- {
		if f.msgs[i].chat == chat {
			return f.msgs[i].text
		}
	}
	return ""
}

type fixture struct {
	dir   string
	store *access.Store
	send  *fakeSender
	logs  *bytes.Buffer
	bot   *bot.Bot
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), send: &fakeSender{}, logs: &bytes.Buffer{}}
	var err error
	f.store, err = access.Open(filepath.Join(f.dir, "allowlist.json"), []int64{admin}, time.Now)
	require.NoError(t, err)
	f.bot = bot.New(f.store, f.send, botName, f.dir, slog.New(slog.NewTextHandler(f.logs, nil)))
	return f
}

// say sends text in user's private chat and returns the reply there.
func (f *fixture) say(user int64, text string) string {
	n := len(f.send.msgs)
	f.bot.Handle(context.Background(), bot.Update{ChatID: user, Private: true, UserID: user, FirstName: "Someone", Text: text})
	return f.send.lastTo(user, n)
}

// sayInGroup sends text in the group chat and returns the reply there.
func (f *fixture) sayInGroup(user int64, text string) string {
	n := len(f.send.msgs)
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
	require.GreaterOrEqual(t, len(f.send.msgs), 2)
	assert.Equal(t, sent{member, bot.MsgWelcome}, f.send.msgs[len(f.send.msgs)-2])
	assert.Equal(t, sent{admin, "Newcomer joined through an invite (user id 7). /revoke 7 removes them."}, f.send.msgs[len(f.send.msgs)-1])
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
	dir := bot.UserDir(f.dir, member)
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
