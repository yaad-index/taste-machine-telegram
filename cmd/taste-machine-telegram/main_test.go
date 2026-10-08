package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/schema"

	"github.com/yaad-index/taste-machine-telegram/bot"
	"github.com/yaad-index/taste-machine-telegram/config"
)

const token = "123:SECRET-TOKEN"

// TestMain lets the test binary stand in for the engine's compile
// command: run with FAKE_COMPILE=1 it writes a small shelf and taste file
// into --out, and fails if the bot token reached its environment.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_COMPILE") == "1" {
		os.Exit(fakeCompile(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeCompile(args []string) int {
	if os.Getenv("FAKE_COMPILE_SLEEP") != "" {
		time.Sleep(time.Minute)
	}
	if os.Getenv(config.EnvToken) != "" {
		fmt.Fprintln(os.Stderr, "taste-machine: error: the bot token leaked into the compile")
		return 1
	}
	out := ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--out="); ok {
			out = v
		}
	}
	sch := schema.Schema{Fields: []schema.Field{{Name: "theme", Type: schema.Category, Role: schema.Preference}}}
	shelf := &fileformat.Catalogue{
		Meta:  fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindCatalogue, Schema: sch},
		Items: []fileformat.Item{{ID: "a", Facts: map[string]schema.Value{"theme": {Type: schema.Category, Category: "sea"}}}},
	}
	rating := 8.0
	taste := &fileformat.Taste{
		Meta:  fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindTaste},
		Items: []fileformat.TasteItem{{ID: "a", Rating: &rating}},
	}
	if err := errors.Join(fileformat.WriteFile(filepath.Join(out, "shelf.zip"), shelf), fileformat.WriteFile(filepath.Join(out, "taste.zip"), taste)); err != nil {
		fmt.Fprintln(os.Stderr, "taste-machine: error:", err)
		return 1
	}
	return 0
}

type testEnv struct {
	stdout, stderr bytes.Buffer
	vars           map[string]string
	environ        []string
	options        []tgbot.Option
}

func (te *testEnv) env() env {
	return env{
		stdout: &te.stdout, stderr: &te.stderr,
		getenv:        func(k string) string { return te.vars[k] },
		environ:       func() []string { return te.environ },
		clientOptions: te.options,
	}
}

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		te := &testEnv{}
		assert.Equal(t, 0, run(context.Background(), []string{arg}, te.env()), arg)
		assert.Equal(t, "taste-machine-telegram dev\n", te.stdout.String(), arg)
		assert.Empty(t, te.stderr.String(), arg)
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"pick"}, {"version", "extra"}} {
		te := &testEnv{}
		assert.Equal(t, 2, run(context.Background(), args, te.env()), args)
		assert.Empty(t, te.stdout.String(), args)
		assert.Equal(t, usage+"\n", te.stderr.String(), args)
	}
}

func TestServeNeedsConfig(t *testing.T) {
	te := &testEnv{}
	assert.Equal(t, 1, run(context.Background(), []string{"serve"}, te.env()))
	assert.Contains(t, te.stderr.String(), config.EnvToken+" is not set")
}

// fakeAPI answers the Bot API methods serve uses: one update, a private
// "/help" from the admin, and records what the bot sends.
type fakeAPI struct {
	mu      sync.Mutex
	text    string
	served  bool
	sent    []string
	edited  []string
	replied chan struct{}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "getMe":
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Bot","username":"TestBot"}}`)
	case "getUpdates":
		if f.served {
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			return
		}
		f.served = true
		text := f.text
		if text == "" {
			text = "/help"
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":5,"date":0,"text":%q,"chat":{"id":1,"type":"private"},"from":{"id":1,"is_bot":false,"first_name":"Admin"}}}]}`, text)
	case "sendMessage":
		f.sent = append(f.sent, string(body))
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":6,"date":0,"chat":{"id":1,"type":"private"}}}`)
		select {
		case f.replied <- struct{}{}:
		default:
		}
	case "editMessageText":
		f.edited = append(f.edited, string(body))
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":6,"date":0,"chat":{"id":1,"type":"private"}}}`)
		select {
		case f.replied <- struct{}{}:
		default:
		}
	default:
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}
}

func TestServeAnswersAnUpdate(t *testing.T) {
	api := &fakeAPI{replied: make(chan struct{}, 1)}
	srv := httptest.NewServer(api)
	defer srv.Close()
	te := &testEnv{
		vars:    map[string]string{config.EnvToken: token, config.EnvAdmins: "1", config.EnvDataDir: t.TempDir()},
		options: []tgbot.Option{tgbot.WithServerURL(srv.URL), tgbot.WithHTTPClient(time.Millisecond, srv.Client())},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- run(ctx, []string{"serve"}, te.env()) }()
	select {
	case <-api.replied:
	case <-time.After(10 * time.Second):
		t.Fatal("no reply sent")
	}
	cancel()
	require.Equal(t, 0, <-done, te.stderr.String())

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.sent, 1)
	assert.Contains(t, api.sent[0], "Commands:")
	assert.Contains(t, api.sent[0], "/invite", "the admin's help lists the admin commands")
	assert.Contains(t, te.stderr.String(), "bot=TestBot")
	assert.NotContains(t, te.stderr.String(), "SECRET-TOKEN")
}

func TestServeNeverPrintsTheToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // a closed server: every request fails with a network error
	te := &testEnv{
		vars:    map[string]string{config.EnvToken: token, config.EnvAdmins: "1", config.EnvDataDir: t.TempDir()},
		options: []tgbot.Option{tgbot.WithServerURL(srv.URL)},
	}
	assert.Equal(t, 1, run(context.Background(), []string{"serve"}, te.env()))
	assert.Contains(t, te.stderr.String(), "error:")
	assert.Contains(t, te.stderr.String(), "/bot***/getMe", "the error carries the request URL, masked")
	assert.NotContains(t, te.stderr.String(), "SECRET-TOKEN")
}

func TestRedact(t *testing.T) {
	assert.Equal(t, "call to /bot***/getMe failed", redact(errors.New("call to /bot"+token+"/getMe failed"), token))
}

func TestConvert(t *testing.T) {
	person := &models.User{ID: 7, FirstName: "Someone"}
	u, ok := convert(&models.Update{Message: &models.Message{Text: "/help", From: person, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate}}})
	require.True(t, ok)
	assert.Equal(t, bot.Update{ChatID: 7, Private: true, UserID: 7, FirstName: "Someone", Text: "/help"}, u)

	u, ok = convert(&models.Update{Message: &models.Message{Text: "/help", From: person, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup}}})
	require.True(t, ok)
	assert.False(t, u.Private, "a group chat is not private")

	for name, upd := range map[string]*models.Update{
		"no message": {},
		"no sender":  {Message: &models.Message{Text: "/help", Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate}}},
		"a bot":      {Message: &models.Message{Text: "/help", From: &models.User{ID: 8, IsBot: true}, Chat: models.Chat{ID: 8, Type: models.ChatTypePrivate}}},
	} {
		_, ok := convert(upd)
		assert.False(t, ok, name)
	}
}

func TestServeLinksThroughTheCompileCommand(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	api := &fakeAPI{text: "/link src someone", replied: make(chan struct{}, 4)}
	srv := httptest.NewServer(api)
	defer srv.Close()
	te := &testEnv{
		vars: map[string]string{config.EnvToken: token, config.EnvAdmins: "1", config.EnvDataDir: t.TempDir(), config.EnvCompile: self},
		// The process environment as serve sees it, token included: serve
		// must strip it before running the compile.
		environ: []string{"FAKE_COMPILE=1", config.EnvToken + "=" + token},
		options: []tgbot.Option{tgbot.WithServerURL(srv.URL), tgbot.WithHTTPClient(time.Millisecond, srv.Client())},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- run(ctx, []string{"serve"}, te.env()) }()
	require.Eventually(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.edited) > 0
	}, 20*time.Second, 10*time.Millisecond)
	cancel()
	require.Equal(t, 0, <-done, te.stderr.String())

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.sent, 1)
	assert.Contains(t, api.sent[0], "Compiling")
	assert.Contains(t, api.edited[0], "Done: 1 items on your shelf, 1 of your items rated.")
	assert.Contains(t, api.edited[0], "name=\"message_id\"\r\n\r\n6\r\n", "the edit names the \"compiling…\" message")
}

// Stopping the bot during a compile stops the command, and serve returns
// only after that compile's reply has been edited.
func TestServeWaitsForTheQueueOnShutdown(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	api := &fakeAPI{text: "/link src someone", replied: make(chan struct{}, 4)}
	srv := httptest.NewServer(api)
	defer srv.Close()
	te := &testEnv{
		vars:    map[string]string{config.EnvToken: token, config.EnvAdmins: "1", config.EnvDataDir: t.TempDir(), config.EnvCompile: self},
		environ: []string{"FAKE_COMPILE=1", "FAKE_COMPILE_SLEEP=1"},
		options: []tgbot.Option{tgbot.WithServerURL(srv.URL), tgbot.WithHTTPClient(time.Millisecond, srv.Client())},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- run(ctx, []string{"serve"}, te.env()) }()
	require.Eventually(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.sent) > 0
	}, 20*time.Second, 10*time.Millisecond)
	cancel()
	require.Equal(t, 0, <-done, te.stderr.String())

	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.edited, 1, "the reply was edited before serve returned")
	assert.Contains(t, api.edited[0], "The compile failed: the compile was stopped")
}

func TestCompileEnv(t *testing.T) {
	env, secrets := compileEnv([]string{
		"PATH=/bin",
		config.EnvToken + "=" + token,
		"TASTE_MACHINE_SOURCE_API_KEY=k-1",
		"OTHER_TOKEN=t-2",
		"EMPTY_KEY=",
		"KEYBOARD=us",
	}, token)
	assert.Equal(t, []string{"PATH=/bin", "TASTE_MACHINE_SOURCE_API_KEY=k-1", "OTHER_TOKEN=t-2", "EMPTY_KEY=", "KEYBOARD=us"}, env, "everything but the bot token")
	assert.Equal(t, []string{token, "k-1", "t-2"}, secrets)
}
