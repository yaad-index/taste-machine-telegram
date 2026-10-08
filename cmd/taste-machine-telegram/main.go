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
	"github.com/yaad-index/taste-machine-telegram/compile"
	"github.com/yaad-index/taste-machine-telegram/config"
	"github.com/yaad-index/taste-machine-telegram/flow"
	"github.com/yaad-index/taste-machine-telegram/userfiles"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

// env is what run takes from the process, so tests can replace it.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	environ        func() []string
	// clientOptions are added to the Telegram client's options.
	clientOptions []tgbot.Option
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], env{stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, environ: os.Environ}))
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
	files := userfiles.New(cfg.DataDir)
	var b *bot.Bot
	childEnv, secrets := compileEnv(e.environ(), cfg.Token)
	queue := compile.NewQueue(compile.CLI{Path: cfg.Compile, Env: childEnv, Secrets: secrets}, files, store.UserAllowed,
		func(ctx context.Context, j compile.Job, sum userfiles.Summary, err error) {
			b.CompileDone(ctx, j, sum, err)
		})
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
	b = bot.New(bot.Deps{Access: store, Files: files, Queue: queue, Send: sender{client}, Name: me.Username, Log: log, Now: time.Now})
	log.Info("started", "version", version, "bot", me.Username)
	queueDone := make(chan struct{})
	go func() { queue.Run(ctx); close(queueDone) }()
	client.Start(ctx)
	<-queueDone
	return nil
}

// compileEnv is the compile command's environment: this process's, less
// the bot token. secrets are the values masked in compile errors: the
// token, and any variable whose name ends in _KEY, _TOKEN, _SECRET or
// _PASSWORD, which is where a source's API key lives.
func compileEnv(environ []string, token string) (env, secrets []string) {
	secrets = []string{token}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if name == config.EnvToken {
			continue
		}
		env = append(env, kv)
		for _, suffix := range []string{"_KEY", "_TOKEN", "_SECRET", "_PASSWORD"} {
			if strings.HasSuffix(name, suffix) && value != "" {
				secrets = append(secrets, value)
				break
			}
		}
	}
	return env, secrets
}

// convert keeps the updates the bot answers: text from a person, and taps
// on the bot's buttons.
func convert(u *models.Update) (bot.Update, bool) {
	if cq := u.CallbackQuery; cq != nil {
		m := cq.Message.Message
		if m == nil || cq.From.IsBot {
			return bot.Update{}, false
		}
		return bot.Update{
			ChatID:     m.Chat.ID,
			Private:    m.Chat.Type == models.ChatTypePrivate,
			UserID:     cq.From.ID,
			FirstName:  cq.From.FirstName,
			Username:   cq.From.Username,
			CallbackID: cq.ID,
			Data:       cq.Data,
			MessageID:  m.ID,
		}, true
	}
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot {
		return bot.Update{}, false
	}
	return bot.Update{
		ChatID:    m.Chat.ID,
		Private:   m.Chat.Type == models.ChatTypePrivate,
		UserID:    m.From.ID,
		FirstName: m.From.FirstName,
		Username:  m.From.Username,
		Text:      m.Text,
	}, true
}

type sender struct{ c *tgbot.Bot }

func (s sender) Send(ctx context.Context, chatID int64, text string) (int, error) {
	m, err := s.c.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text})
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

func (s sender) Edit(ctx context.Context, chatID int64, messageID int, text string) error {
	_, err := s.c.EditMessageText(ctx, &tgbot.EditMessageTextParams{ChatID: chatID, MessageID: messageID, Text: text})
	return err
}

func (s sender) SendScreen(ctx context.Context, chatID int64, sc flow.Screen) (int, error) {
	m, err := s.c.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: sc.Text, ReplyMarkup: keyboard(sc)})
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

// EditScreen replaces text and buttons; a screen without buttons removes
// the message's buttons, since an edit without a keyboard drops it.
func (s sender) EditScreen(ctx context.Context, chatID int64, messageID int, sc flow.Screen) error {
	_, err := s.c.EditMessageText(ctx, &tgbot.EditMessageTextParams{ChatID: chatID, MessageID: messageID, Text: sc.Text, ReplyMarkup: keyboard(sc)})
	return err
}

func (s sender) AnswerTap(ctx context.Context, callbackID, text string) error {
	_, err := s.c.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{CallbackQueryID: callbackID, Text: text})
	return err
}

// keyboard is a screen's buttons as an inline keyboard, or nil (no
// keyboard at all) when it has none.
func keyboard(sc flow.Screen) models.ReplyMarkup {
	if len(sc.Buttons) == 0 {
		return nil
	}
	rows := make([][]models.InlineKeyboardButton, 0, len(sc.Buttons))
	for _, row := range sc.Buttons {
		r := make([]models.InlineKeyboardButton, 0, len(row))
		for _, b := range row {
			r = append(r, models.InlineKeyboardButton{Text: b.Text, CallbackData: b.Data})
		}
		rows = append(rows, r)
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// redact renders err with the bot token masked. The client masks it in
// the URLs it reports; this covers any other place it might appear.
func redact(err error, token string) string {
	return strings.ReplaceAll(err.Error(), token, "***")
}
