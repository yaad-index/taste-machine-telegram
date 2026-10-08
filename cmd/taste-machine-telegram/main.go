// Command taste-machine-telegram is a Telegram frontend for the
// taste-machine engine.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/yaad-index/taste-machine-telegram/access"
	"github.com/yaad-index/taste-machine-telegram/bot"
	"github.com/yaad-index/taste-machine-telegram/config"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

// env is what run takes from the process, so tests can replace it.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	// clientOptions are added to the Telegram client's options.
	clientOptions []tgbot.Option
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], env{stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}))
}

const usage = "usage: taste-machine-telegram serve | version"

func run(ctx context.Context, args []string, e env) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(e.stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "-version", "--version":
		_, _ = fmt.Fprintln(e.stdout, "taste-machine-telegram", version)
		return 0
	case "serve":
		if err := serve(ctx, e); err != nil {
			_, _ = fmt.Fprintln(e.stderr, "error:", err)
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintln(e.stderr, usage)
		return 2
	}
}

// serve runs the bot with long polling until ctx ends.
func serve(ctx context.Context, e env) error {
	cfg, err := config.Load(e.getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(e.stderr, nil))
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	store, err := access.Open(filepath.Join(cfg.DataDir, "allowlist.json"), cfg.Admins, time.Now)
	if err != nil {
		return err
	}
	var b *bot.Bot
	opts := append([]tgbot.Option{
		// Handlers run on the client's workers, which Start waits for, so
		// serve returns only after every reply in flight is done.
		tgbot.WithNotAsyncHandlers(),
		tgbot.WithDefaultHandler(func(ctx context.Context, _ *tgbot.Bot, u *models.Update) {
			if upd, ok := convert(u); ok {
				b.Handle(ctx, upd)
			}
		}),
		tgbot.WithErrorsHandler(func(err error) {
			log.Error("telegram client", "err", redact(err, cfg.Token))
		}),
	}, e.clientOptions...)
	client, err := tgbot.New(cfg.Token, opts...)
	if err != nil {
		return errors.New(redact(err, cfg.Token))
	}
	me, err := client.GetMe(ctx)
	if err != nil {
		return errors.New(redact(err, cfg.Token))
	}
	b = bot.New(store, sender{client}, me.Username, cfg.DataDir, log)
	log.Info("started", "version", version, "bot", me.Username)
	client.Start(ctx)
	return nil
}

// convert keeps the messages the bot answers: text from a person.
func convert(u *models.Update) (bot.Update, bool) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot {
		return bot.Update{}, false
	}
	return bot.Update{
		ChatID:    m.Chat.ID,
		Private:   m.Chat.Type == models.ChatTypePrivate,
		UserID:    m.From.ID,
		FirstName: m.From.FirstName,
		Text:      m.Text,
	}, true
}

type sender struct{ c *tgbot.Bot }

func (s sender) Send(ctx context.Context, chatID int64, text string) error {
	_, err := s.c.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text})
	return err
}

// redact renders err with the bot token masked. The client masks it in
// the URLs it reports; this covers any other place it might appear.
func redact(err error, token string) string {
	return strings.ReplaceAll(err.Error(), token, "***")
}
