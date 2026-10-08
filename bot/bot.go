// Package bot routes chat commands and applies the access rules of ADR 0001
// section 2. It works on its own small Update type and a Sender, so it does
// not depend on the Telegram client.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yaad-index/taste-machine-telegram/access"
)

// Update is one incoming message.
type Update struct {
	ChatID int64
	// Private is true in a one-to-one chat with the bot.
	Private   bool
	UserID    int64
	FirstName string
	Text      string
}

// Sender sends a text message to a chat.
type Sender interface {
	Send(ctx context.Context, chatID int64, text string) error
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
)

// Bot handles updates.
type Bot struct {
	access  *access.Store
	send    Sender
	name    string
	dataDir string
	log     *slog.Logger
}

// New returns a bot. name is the bot's username, used in invite links and
// to recognise commands addressed to it in group chats.
func New(store *access.Store, send Sender, name, dataDir string, log *slog.Logger) *Bot {
	return &Bot{access: store, send: send, name: name, dataDir: dataDir, log: log}
}

// UserDir is where a user's files live.
func UserDir(dataDir string, user int64) string {
	return filepath.Join(dataDir, "users", strconv.FormatInt(user, 10))
}

// Handle answers one update. Nothing about a user who is not allowed is
// logged or stored.
func (b *Bot) Handle(ctx context.Context, u Update) {
	cmd, arg, ok := b.command(u.Text)
	if u.Private {
		b.private(ctx, u, cmd, arg, ok)
	} else if ok {
		b.group(ctx, u, cmd, arg)
	}
}

func (b *Bot) private(ctx context.Context, u Update, cmd, arg string, isCmd bool) {
	if !b.access.UserAllowed(u.UserID) {
		if cmd == "start" && arg != "" {
			b.redeem(ctx, u, arg)
			return
		}
		_ = b.send.Send(ctx, u.ChatID, MsgRefused)
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
	case "allow", "disallow":
		b.reply(ctx, u, MsgInGroupOnly)
	default:
		b.reply(ctx, u, MsgUnknown)
	}
}

func (b *Bot) group(ctx context.Context, u Update, cmd, arg string) {
	if !b.access.UserAllowed(u.UserID) {
		_ = b.send.Send(ctx, u.ChatID, MsgRefused)
		return
	}
	switch cmd {
	case "allow", "disallow":
		b.allow(ctx, u, cmd == "allow")
		return
	}
	if !b.access.ChatAllowed(u.ChatID) {
		b.reply(ctx, u, MsgChatNotAllowed)
		return
	}
	switch cmd {
	case "start", "help":
		b.reply(ctx, u, b.help(u.UserID))
	case "invite", "revoke":
		b.reply(ctx, u, MsgInPrivateOnly)
	default:
		b.reply(ctx, u, MsgUnknown)
	}
}

func (b *Bot) redeem(ctx context.Context, u Update, token string) {
	if err := b.access.Redeem(token, u.UserID); err != nil {
		if !errors.Is(err, access.ErrInvalidInvite) {
			b.log.Error("redeeming an invite", "err", err)
		}
		_ = b.send.Send(ctx, u.ChatID, MsgInviteInvalid)
		return
	}
	b.reply(ctx, u, MsgWelcome)
	note := fmt.Sprintf("%s joined through an invite (user id %d). /revoke %d removes them.", u.FirstName, u.UserID, u.UserID)
	for _, admin := range b.access.Admins() {
		if err := b.send.Send(ctx, admin, note); err != nil {
			b.log.Error("telling an admin about a new user", "err", err)
		}
	}
}

func (b *Bot) invite(ctx context.Context, u Update) {
	if !b.access.IsAdmin(u.UserID) {
		b.reply(ctx, u, MsgAdminOnly)
		return
	}
	token, expires, err := b.access.Invite()
	if err != nil {
		b.log.Error("creating an invite", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	b.reply(ctx, u, fmt.Sprintf("One-time invite, valid until %s:\nhttps://t.me/%s?start=%s",
		expires.UTC().Format("2006-01-02 15:04 UTC"), b.name, token))
}

func (b *Bot) revoke(ctx context.Context, u Update, arg string) {
	if !b.access.IsAdmin(u.UserID) {
		b.reply(ctx, u, MsgAdminOnly)
		return
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		b.reply(ctx, u, MsgRevokeUsage)
		return
	}
	found, err := b.access.Revoke(id)
	switch {
	case errors.Is(err, access.ErrAdmin):
		b.reply(ctx, u, "Admins come from the configuration and cannot be revoked.")
		return
	case err != nil:
		b.log.Error("revoking a user", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	if err := os.RemoveAll(UserDir(b.dataDir, id)); err != nil {
		b.log.Error("deleting a revoked user's files", "err", err)
		b.reply(ctx, u, fmt.Sprintf("User %d is revoked, but deleting their files failed.", id))
		return
	}
	if !found {
		b.reply(ctx, u, fmt.Sprintf("User %d was not on the allowlist; any files they had are deleted.", id))
		return
	}
	b.reply(ctx, u, fmt.Sprintf("User %d is revoked and their files are deleted.", id))
}

func (b *Bot) allow(ctx context.Context, u Update, allow bool) {
	if !b.access.IsAdmin(u.UserID) {
		b.reply(ctx, u, MsgAdminOnly)
		return
	}
	change, msg := b.access.Disallow, MsgChatDisallowed
	if allow {
		change, msg = b.access.Allow, MsgChatAllowed
	}
	if err := change(u.ChatID); err != nil {
		b.log.Error("changing the chat allowlist", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	b.reply(ctx, u, msg)
}

func (b *Bot) help(user int64) string {
	lines := []string{"Commands:", "/help shows this list."}
	if b.access.IsAdmin(user) {
		lines = append(lines,
			"/invite creates a one-time invite link (private chat).",
			"/revoke <user id> removes a user and deletes their files (private chat).",
			"/allow and /disallow let me answer in a group chat, or stop (run them in the group).")
	}
	return strings.Join(lines, "\n")
}

// reply answers an allowed user; a failed send is logged without content.
func (b *Bot) reply(ctx context.Context, u Update, text string) {
	if err := b.send.Send(ctx, u.ChatID, text); err != nil {
		b.log.Error("sending a reply", "err", err)
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
	if addressed && !strings.EqualFold(target, b.name) {
		return "", "", false
	}
	if name == "" {
		return "", "", false
	}
	return strings.ToLower(name), strings.TrimSpace(arg), true
}
