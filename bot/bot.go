// Package bot routes chat commands and applies the access rules of ADR 0001
// section 2. It works on its own small Update type and a Sender, so it does
// not depend on the Telegram client.
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/taste-machine/dataset"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/score"

	"github.com/yaad-index/taste-machine-telegram/access"
	"github.com/yaad-index/taste-machine-telegram/compile"
	"github.com/yaad-index/taste-machine-telegram/flow"
	"github.com/yaad-index/taste-machine-telegram/userfiles"
)

// Update is one incoming message, or a tap on a button.
type Update struct {
	ChatID int64
	// Private is true in a one-to-one chat with the bot.
	Private   bool
	UserID    int64
	FirstName string
	Username  string
	Text      string
	// CallbackID is set for a tap on a button; Data is the button's data
	// and MessageID the message the button is on.
	CallbackID string
	Data       string
	MessageID  int
}

// Sender sends messages, edits them and answers taps on buttons.
type Sender interface {
	// Send returns the sent message's id.
	Send(ctx context.Context, chatID int64, text string) (int, error)
	Edit(ctx context.Context, chatID int64, messageID int, text string) error
	// SendScreen sends a text with buttons and returns its id.
	SendScreen(ctx context.Context, chatID int64, s flow.Screen) (int, error)
	// EditScreen replaces a message's text and buttons.
	EditScreen(ctx context.Context, chatID int64, messageID int, s flow.Screen) error
	// AnswerTap acknowledges a tap, with a short notice when text is set.
	AnswerTap(ctx context.Context, callbackID, text string) error
}

// Replies the bot sends.
const (
	MsgRefused         = "This bot is invite-only."
	MsgInviteInvalid   = "This invite link is not valid: it was used or it expired."
	MsgWelcome         = "Welcome. /help lists what I can do."
	MsgAdminOnly       = "Only an admin can do that."
	MsgUnknown         = "Unknown command. /help lists what I can do."
	MsgChatNotAllowed  = "This chat is not allowed. An admin can run /allow here."
	MsgInGroupOnly     = "Run this in the group chat it is for."
	MsgInPrivateOnly   = "Run this in a private chat with me."
	MsgChatAllowed     = "This chat is allowed now."
	MsgChatDisallowed  = "This chat is no longer allowed."
	MsgRevokeUsage     = "Usage: /revoke <user id>"
	MsgSomethingFailed = "Something went wrong; try again later."
	MsgLinkUsage       = "Usage: /link <source> <user name>"
	MsgNotLinked       = "Link an account first: /link <source> <user name>"
	MsgCompiling       = "Compiling… this can take a minute."
	MsgAlreadyBusy     = "A compile of yours is already running; wait for it to finish."
	MsgNothingToUnlink = "You have no linked account."
	MsgUnlinked        = "Your account is unlinked and your files are deleted."
	MsgSessionEnded    = "This session has ended (the bot restarted or it timed out); start again with /pick, or /night in a group."
	MsgStaleTap        = "That question was already answered."
)

// SessionTTL is how long a pick session lasts.
const SessionTTL = 6 * time.Hour

// CompileEvery is the shortest time between two compiles of one user.
const CompileEvery = time.Hour

// Deps are what a Bot works with.
type Deps struct {
	Access *access.Store
	Files  *userfiles.Store
	// Queue runs compiles; its Done should be the bot's CompileDone.
	Queue *compile.Queue
	Send  Sender
	// Name is the bot's username, used in invite links and to recognise
	// commands addressed to it in group chats.
	Name string
	Log  *slog.Logger
	Now  func() time.Time
}

// Bot handles updates.
type Bot struct {
	Deps
	// solo holds each user's pick session in their private chat. Updates
	// are handled one at a time, but the lock keeps that an assumption of
	// the transport rather than of this package.
	mu   sync.Mutex
	solo map[int64]*session
	// nights holds each group chat's session.
	nights map[int64]*night
}

type session struct {
	flow    *flow.Session
	started time.Time
}

// New returns a bot.
func New(d Deps) *Bot {
	return &Bot{Deps: d, solo: map[int64]*session{}, nights: map[int64]*night{}}
}

// Handle answers one update. Nothing about a user who is not allowed is
// logged or stored.
func (b *Bot) Handle(ctx context.Context, u Update) {
	if u.CallbackID != "" {
		b.tap(ctx, u)
		return
	}
	cmd, arg, ok := b.command(u.Text)
	if u.Private {
		b.private(ctx, u, cmd, arg, ok)
	} else if ok {
		b.group(ctx, u, cmd, arg)
	}
}

func (b *Bot) private(ctx context.Context, u Update, cmd, arg string, isCmd bool) {
	if !b.Access.UserAllowed(u.UserID) {
		if cmd == "start" && arg != "" {
			b.redeem(ctx, u, arg)
			return
		}
		_, _ = b.Send.Send(ctx, u.ChatID, MsgRefused)
		return
	}
	if !isCmd {
		b.reply(ctx, u, b.help(u.UserID))
		return
	}
	switch cmd {
	case "start", "help":
		b.reply(ctx, u, b.help(u.UserID))
	case "invite":
		b.invite(ctx, u)
	case "revoke":
		b.revoke(ctx, u, arg)
	case "link":
		b.link(ctx, u, arg)
	case "refresh":
		b.refresh(ctx, u)
	case "unlink":
		b.unlink(ctx, u)
	case "pick":
		b.pick(ctx, u)
	case "allow", "disallow":
		b.reply(ctx, u, MsgInGroupOnly)
	default:
		b.reply(ctx, u, MsgUnknown)
	}
}

func (b *Bot) group(ctx context.Context, u Update, cmd, arg string) {
	if !b.Access.UserAllowed(u.UserID) {
		_, _ = b.Send.Send(ctx, u.ChatID, MsgRefused)
		return
	}
	switch cmd {
	case "allow", "disallow":
		b.allow(ctx, u, cmd == "allow")
		return
	}
	if !b.Access.ChatAllowed(u.ChatID) {
		b.reply(ctx, u, MsgChatNotAllowed)
		return
	}
	switch cmd {
	case "start", "help":
		b.reply(ctx, u, b.help(u.UserID))
	case "night":
		b.openNight(ctx, u)
	case "pick":
		b.groupPick(ctx, u)
	case "done":
		b.endNight(ctx, u)
	case "invite", "revoke", "link", "refresh", "unlink":
		b.reply(ctx, u, MsgInPrivateOnly)
	default:
		b.reply(ctx, u, MsgUnknown)
	}
}

func (b *Bot) redeem(ctx context.Context, u Update, token string) {
	if err := b.Access.Redeem(token, u.UserID); err != nil {
		if !errors.Is(err, access.ErrInvalidInvite) {
			b.Log.Error("redeeming an invite", "err", err)
		}
		_, _ = b.Send.Send(ctx, u.ChatID, MsgInviteInvalid)
		return
	}
	b.reply(ctx, u, MsgWelcome)
	note := fmt.Sprintf("%s joined through an invite (user id %d). /revoke %d removes them.", u.FirstName, u.UserID, u.UserID)
	for _, admin := range b.Access.Admins() {
		if _, err := b.Send.Send(ctx, admin, note); err != nil {
			b.Log.Error("telling an admin about a new user", "err", err)
		}
	}
}

func (b *Bot) invite(ctx context.Context, u Update) {
	if !b.Access.IsAdmin(u.UserID) {
		b.reply(ctx, u, MsgAdminOnly)
		return
	}
	token, expires, err := b.Access.Invite()
	if err != nil {
		b.Log.Error("creating an invite", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	b.reply(ctx, u, fmt.Sprintf("One-time invite, valid until %s:\nhttps://t.me/%s?start=%s",
		expires.UTC().Format("2006-01-02 15:04 UTC"), b.Name, token))
}

func (b *Bot) revoke(ctx context.Context, u Update, arg string) {
	if !b.Access.IsAdmin(u.UserID) {
		b.reply(ctx, u, MsgAdminOnly)
		return
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		b.reply(ctx, u, MsgRevokeUsage)
		return
	}
	found, err := b.Access.Revoke(id)
	switch {
	case errors.Is(err, access.ErrAdmin):
		b.reply(ctx, u, "Admins come from the configuration and cannot be revoked.")
		return
	case err != nil:
		b.Log.Error("revoking a user", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	if err := b.Files.Remove(id); err != nil {
		b.Log.Error("deleting a revoked user's files", "err", err)
		b.reply(ctx, u, fmt.Sprintf("User %d is revoked, but deleting their files failed.", id))
		return
	}
	b.leaveNights(ctx, id)
	if !found {
		b.reply(ctx, u, fmt.Sprintf("User %d was not on the allowlist; any files they had are deleted.", id))
		return
	}
	b.reply(ctx, u, fmt.Sprintf("User %d is revoked and their files are deleted.", id))
}

func (b *Bot) allow(ctx context.Context, u Update, allow bool) {
	if !b.Access.IsAdmin(u.UserID) {
		b.reply(ctx, u, MsgAdminOnly)
		return
	}
	change, msg := b.Access.Disallow, MsgChatDisallowed
	if allow {
		change, msg = b.Access.Allow, MsgChatAllowed
	}
	if err := change(u.ChatID); err != nil {
		b.Log.Error("changing the chat allowlist", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	b.reply(ctx, u, msg)
}

func (b *Bot) help(user int64) string {
	lines := []string{
		"Commands:",
		"/help shows this list.",
		"/link <source> <user name> compiles your shelf and taste from a source account (private chat).",
		"/refresh compiles them again; at most once an hour (private chat).",
		"/unlink deletes your files (private chat).",
		"/pick asks a few questions and suggests what to pick from your shelf (private chat).",
		"/night opens a session in a group chat: members tap I'm in, then /pick asks the group, on the shelf of whoever runs it. /done ends the session.",
	}
	if b.Access.IsAdmin(user) {
		lines = append(lines,
			"/invite creates a one-time invite link (private chat).",
			"/revoke <user id> removes a user and deletes their files (private chat).",
			"/allow and /disallow let me answer in a group chat, or stop (run them in the group).")
	}
	return strings.Join(lines, "\n")
}

func (b *Bot) link(ctx context.Context, u Update, arg string) {
	source, name, _ := strings.Cut(arg, " ")
	l := userfiles.Link{Source: strings.ToLower(source), User: strings.TrimSpace(name)}
	if err := compile.ValidateLink(l); err != nil {
		b.reply(ctx, u, MsgLinkUsage+"\n"+err.Error())
		return
	}
	b.startCompile(ctx, u, l)
}

func (b *Bot) refresh(ctx context.Context, u Update) {
	l, err := b.Files.Link(u.UserID)
	switch {
	case errors.Is(err, userfiles.ErrNotLinked):
		b.reply(ctx, u, MsgNotLinked)
		return
	case err != nil:
		b.Log.Error("reading a link", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	b.startCompile(ctx, u, l)
}

// startCompile applies the busy check and the hourly limit, replies
// "compiling…" and queues the compile; CompileDone edits that reply.
func (b *Bot) startCompile(ctx context.Context, u Update, l userfiles.Link) {
	if b.Queue.Busy(u.UserID) {
		b.reply(ctx, u, MsgAlreadyBusy)
		return
	}
	if last, ok := b.Files.LastCompile(u.UserID); ok {
		if next := last.Add(CompileEvery); b.Now().Before(next) {
			b.reply(ctx, u, fmt.Sprintf("You can compile again after %s.", next.UTC().Format("15:04 UTC")))
			return
		}
	}
	msg, err := b.Send.Send(ctx, u.ChatID, MsgCompiling)
	if err != nil {
		b.Log.Error("sending a reply", "err", err)
		return
	}
	if !b.Queue.Add(compile.Job{User: u.UserID, Link: l, Chat: u.ChatID, Message: msg}) {
		b.edit(ctx, u.ChatID, msg, MsgAlreadyBusy)
	}
}

// CompileDone reports a finished compile by editing its "compiling…" reply.
func (b *Bot) CompileDone(ctx context.Context, j compile.Job, sum userfiles.Summary, err error) {
	text := fmt.Sprintf("Done: %d items on your shelf, %d of your items rated.", sum.Shelf, sum.Rated)
	if err != nil {
		text = "The compile failed: " + err.Error()
	}
	b.edit(ctx, j.Chat, j.Message, text)
}

func (b *Bot) unlink(ctx context.Context, u Update) {
	if b.Queue.Busy(u.UserID) {
		b.reply(ctx, u, MsgAlreadyBusy)
		return
	}
	if _, err := b.Files.Link(u.UserID); errors.Is(err, userfiles.ErrNotLinked) {
		b.reply(ctx, u, MsgNothingToUnlink)
		return
	}
	if err := b.Files.Remove(u.UserID); err != nil {
		b.Log.Error("deleting a user's files", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	b.reply(ctx, u, MsgUnlinked)
}

// pick starts a question flow on the user's own shelf, replacing any
// session they had.
func (b *Bot) pick(ctx context.Context, u Update) {
	taste, err := b.load(u.UserID)
	switch {
	case errors.Is(err, userfiles.ErrNotLinked):
		b.reply(ctx, u, MsgNotLinked)
		return
	case err != nil:
		b.Log.Error("loading a user's files", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	s := flow.New(sessionID(), taste)
	b.mu.Lock()
	b.solo[u.UserID] = &session{flow: s, started: b.Now()}
	b.mu.Unlock()
	if _, err := b.Send.SendScreen(ctx, u.ChatID, s.Start()); err != nil {
		b.Log.Error("sending a question", "err", err)
	}
}

// load reads the user's current files into a taste.
func (b *Bot) load(user int64) (score.Taste, error) {
	shelfPath, tastePath, err := b.Files.Paths(user)
	if err != nil {
		return nil, err
	}
	shelf, err := fileformat.ReadCatalogueFile(shelfPath)
	if err != nil {
		return nil, err
	}
	taste, err := fileformat.ReadTasteFile(tastePath)
	if err != nil {
		return nil, err
	}
	d, err := dataset.Load(shelf, []dataset.Input{{Taste: taste, Name: "you"}}, nil)
	if err != nil {
		return nil, err
	}
	return score.Learn(d, d.Members[0]), nil
}

// tap handles a tap on a button: the session's flow decides what the tap
// does, and every tap is acknowledged.
func (b *Bot) tap(ctx context.Context, u Update) {
	if !b.Access.UserAllowed(u.UserID) {
		_ = b.Send.AnswerTap(ctx, u.CallbackID, MsgRefused)
		return
	}
	if !u.Private {
		if !b.Access.ChatAllowed(u.ChatID) {
			b.answerTap(ctx, u, MsgChatNotAllowed)
			return
		}
		b.groupTap(ctx, u)
		return
	}
	s := b.session(u)
	if s == nil {
		b.answerTap(ctx, u, MsgSessionEnded)
		return
	}
	reply, err := s.Tap(u.Data)
	switch {
	case errors.Is(err, flow.ErrStale):
		b.answerTap(ctx, u, MsgStaleTap)
		return
	case err != nil:
		b.answerTap(ctx, u, MsgSessionEnded)
		return
	}
	b.answerTap(ctx, u, "")
	if reply.Edit != nil {
		if err := b.Send.EditScreen(ctx, u.ChatID, u.MessageID, *reply.Edit); err != nil {
			b.Log.Error("editing a question", "err", err)
		}
	}
	b.sendAll(ctx, u.ChatID, reply.Send)
}

// session returns the user's live session, or nil when there is none: it
// timed out or was lost to a restart. A tap on a replaced session's button
// is turned away by the new session, whose id it does not carry.
func (b *Bot) session(u Update) *flow.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.solo[u.UserID]
	if !ok {
		return nil
	}
	if b.Now().Sub(s.started) >= SessionTTL {
		delete(b.solo, u.UserID)
		return nil
	}
	return s.flow
}

func (b *Bot) answerTap(ctx context.Context, u Update, text string) {
	if err := b.Send.AnswerTap(ctx, u.CallbackID, text); err != nil {
		b.Log.Error("answering a tap", "err", err)
	}
}

// sessionID is a short random id that ties buttons to their session.
func sessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (b *Bot) edit(ctx context.Context, chat int64, msg int, text string) {
	if err := b.Send.Edit(ctx, chat, msg, text); err != nil {
		b.Log.Error("editing a reply", "err", err)
	}
}

// reply answers an allowed user; a failed send is logged without content.
func (b *Bot) reply(ctx context.Context, u Update, text string) {
	if _, err := b.Send.Send(ctx, u.ChatID, text); err != nil {
		b.Log.Error("sending a reply", "err", err)
	}
}

// command splits "/cmd@name arg" into its lower-case name and argument. A
// command addressed to another bot is not a command for this one.
func (b *Bot) command(text string) (cmd, arg string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	head, arg, _ := strings.Cut(strings.TrimSpace(text[1:]), " ")
	name, target, addressed := strings.Cut(head, "@")
	if addressed && !strings.EqualFold(target, b.Name) {
		return "", "", false
	}
	if name == "" {
		return "", "", false
	}
	return strings.ToLower(name), strings.TrimSpace(arg), true
}
