package bot

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yaad-index/taste-machine/dataset"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/group"
	"github.com/yaad-index/taste-machine/score"

	"github.com/yaad-index/taste-machine-telegram/flow"
	"github.com/yaad-index/taste-machine-telegram/userfiles"
)

// Replies of the group flow (ADR 0001 section 5).
const (
	MsgNightOpen      = "A session is already open here; /done ends it."
	MsgNoNight        = "No session is open here; /night opens one."
	MsgPickRunning    = "A pick is already running here; /done ends the session."
	MsgJoinFirst      = "Tap I'm in first; /pick uses the shelf of whoever runs it."
	MsgJoined         = "You're in."
	MsgAlreadyIn      = "You're in already."
	MsgLinkFirst      = "Use /link in a private chat first."
	MsgOtherSchema    = "Your files come from a different source than this session's."
	MsgJoinClosed     = "The pick has started; join the next session."
	MsgMembersOnly    = "Only members who joined can answer."
	MsgNightDone      = "Session ended."
	MsgNightNoMembers = "This session ended: no members are left."
)

// night is a group session in one chat.
type night struct {
	id      string
	chat    int64
	started time.Time
	// lobby is the message with the "I'm in" button.
	lobby   int
	members []*member
	// schema is the schema id of the first member's files; every member's
	// must match.
	schema string
	// Set by /pick: the files loaded then, which the session keeps.
	shelf  *fileformat.Catalogue
	tastes map[int64]*fileformat.Taste
	flow   *flow.Session
	// rebuild is set when a member left: the flow moves onto the members
	// left at the next answer.
	rebuild bool
}

type member struct {
	id              int64
	first, username string
	label           string
}

func (n *night) member(id int64) *member {
	for _, m := range n.members {
		if m.id == id {
			return m
		}
	}
	return nil
}

func (n *night) lobbyScreen() flow.Screen {
	names := make([]string, 0, len(n.members))
	for _, m := range n.members {
		names = append(names, m.first)
	}
	in := "nobody yet"
	if len(names) > 0 {
		in = strings.Join(names, ", ")
	}
	return flow.Screen{
		Text:    "Tonight's session. In: " + in + ".\nTap I'm in to join; then /pick starts, on the shelf of whoever runs it.",
		Buttons: [][]flow.Button{{{Text: "I'm in", Data: n.id + " j"}}},
	}
}

// liveNight returns the chat's session, ending it first if it has timed
// out.
func (b *Bot) liveNight(chat int64) *night {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nights[chat]
	if !ok {
		return nil
	}
	if b.Now().Sub(n.started) >= SessionTTL {
		delete(b.nights, chat)
		return nil
	}
	return n
}

func (b *Bot) openNight(ctx context.Context, u Update) {
	if b.liveNight(u.ChatID) != nil {
		b.reply(ctx, u, MsgNightOpen)
		return
	}
	n := &night{id: sessionID(), chat: u.ChatID, started: b.Now()}
	id, err := b.Send.SendScreen(ctx, u.ChatID, n.lobbyScreen())
	if err != nil {
		b.Log.Error("sending a lobby", "err", err)
		return
	}
	n.lobby = id
	b.mu.Lock()
	b.nights[u.ChatID] = n
	b.mu.Unlock()
}

func (b *Bot) endNight(ctx context.Context, u Update) {
	if b.liveNight(u.ChatID) == nil {
		b.reply(ctx, u, MsgNoNight)
		return
	}
	b.mu.Lock()
	delete(b.nights, u.ChatID)
	b.mu.Unlock()
	b.reply(ctx, u, MsgNightDone)
}

func (b *Bot) join(ctx context.Context, u Update, n *night) {
	switch {
	case n.flow != nil:
		b.answerTap(ctx, u, MsgJoinClosed)
		return
	case n.member(u.UserID) != nil:
		b.answerTap(ctx, u, MsgAlreadyIn)
		return
	}
	_, tastePath, err := b.Files.Paths(u.UserID)
	if errors.Is(err, userfiles.ErrNotLinked) {
		b.answerTap(ctx, u, MsgLinkFirst)
		return
	}
	var taste *fileformat.Taste
	if err == nil {
		taste, err = fileformat.ReadTasteFile(tastePath)
	}
	if err != nil {
		b.Log.Error("reading a member's files", "err", err)
		b.answerTap(ctx, u, MsgSomethingFailed)
		return
	}
	if n.schema != "" && taste.Meta.SchemaID != n.schema {
		b.answerTap(ctx, u, MsgOtherSchema)
		return
	}
	n.schema = taste.Meta.SchemaID
	n.members = append(n.members, &member{id: u.UserID, first: u.FirstName, username: u.Username})
	b.answerTap(ctx, u, MsgJoined)
	if err := b.Send.EditScreen(ctx, n.chat, n.lobby, n.lobbyScreen()); err != nil {
		b.Log.Error("editing a lobby", "err", err)
	}
}

// groupPick starts the session's flow on the caller's shelf with every
// member's taste.
func (b *Bot) groupPick(ctx context.Context, u Update) {
	n := b.liveNight(u.ChatID)
	switch {
	case n == nil:
		b.reply(ctx, u, MsgNoNight)
		return
	case n.flow != nil:
		b.reply(ctx, u, MsgPickRunning)
		return
	case n.member(u.UserID) == nil:
		b.reply(ctx, u, MsgJoinFirst)
		return
	}
	shelfPath, _, err := b.Files.Paths(u.UserID)
	var shelf *fileformat.Catalogue
	if err == nil {
		shelf, err = fileformat.ReadCatalogueFile(shelfPath)
	}
	if err != nil {
		b.Log.Error("reading the picker's shelf", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	tastes := map[int64]*fileformat.Taste{}
	var kept []*member
	var left []string
	for _, m := range n.members {
		_, p, err := b.Files.Paths(m.id)
		var t *fileformat.Taste
		if err == nil {
			t, err = fileformat.ReadTasteFile(p)
		}
		if err != nil {
			left = append(left, m.first)
			continue
		}
		tastes[m.id] = t
		kept = append(kept, m)
	}
	n.members, n.shelf, n.tastes = kept, shelf, tastes
	label(n.members)
	taste, err := n.taste()
	if err != nil {
		b.Log.Error("loading a group", "err", err)
		b.reply(ctx, u, MsgSomethingFailed)
		return
	}
	n.flow = flow.New(n.id, taste)
	screen := n.flow.Start()
	if len(left) > 0 {
		screen.Text = "Left out, their files are gone: " + strings.Join(left, ", ") + ".\n\n" + screen.Text
	}
	if _, err := b.Send.SendScreen(ctx, u.ChatID, screen); err != nil {
		b.Log.Error("sending a question", "err", err)
	}
}

// taste builds the members' taste over the loaded shelf: group mode, or
// single mode for one member. Each member is labelled by their label, not
// the label in their taste file.
func (n *night) taste() (score.Taste, error) {
	inputs := make([]dataset.Input, 0, len(n.members))
	for _, m := range n.members {
		t := *n.tastes[m.id]
		meta := fileformat.TasteMeta{}
		if t.Meta.Taste != nil {
			meta = *t.Meta.Taste
		}
		meta.Label = m.label
		t.Meta.Taste = &meta
		inputs = append(inputs, dataset.Input{Taste: &t, Name: m.label})
	}
	d, err := dataset.Load(n.shelf, inputs, nil)
	if err != nil {
		return nil, err
	}
	if len(d.Members) == 1 {
		return score.Learn(d, d.Members[0]), nil
	}
	return group.New(d, 0, score.DefaultAnswerWeight)
}

// label gives each member a unique label: their first name, with their
// username (or id) added when the first name is shared.
func label(ms []*member) {
	count := map[string]int{}
	for _, m := range ms {
		count[m.first]++
	}
	for _, m := range ms {
		switch {
		case m.first != "" && count[m.first] == 1:
			m.label = m.first
		case m.username != "":
			m.label = strings.TrimSpace(m.first + " (@" + m.username + ")")
		default:
			m.label = strings.TrimSpace(m.first + " (" + strconv.FormatInt(m.id, 10) + ")")
		}
	}
}

// groupTap handles a tap in a group chat: joining, or answering.
func (b *Bot) groupTap(ctx context.Context, u Update) {
	n := b.liveNight(u.ChatID)
	if n == nil || !strings.HasPrefix(u.Data, n.id+" ") {
		b.answerTap(ctx, u, MsgSessionEnded)
		return
	}
	if u.Data == n.id+" j" {
		b.join(ctx, u, n)
		return
	}
	m := n.member(u.UserID)
	if m == nil || n.flow == nil {
		b.answerTap(ctx, u, MsgMembersOnly)
		return
	}
	wasDone := n.flow.Done()
	reply, err := n.flow.Tap(u.Data)
	switch {
	case errors.Is(err, flow.ErrStale):
		b.answerTap(ctx, u, MsgStaleTap)
		return
	case err != nil:
		b.answerTap(ctx, u, MsgSessionEnded)
		return
	}
	// A member left: the next screen, question or results, comes from the
	// members left. Results shown before they left stay as they were.
	if n.rebuild && !wasDone {
		if screen, err := b.rebuild(n); err != nil {
			b.Log.Error("rebuilding a group", "err", err)
		} else {
			reply.Send = []flow.Screen{screen}
		}
	}
	b.answerTap(ctx, u, "")
	if reply.Edit != nil {
		edit := *reply.Edit
		edit.Text += " (" + m.label + ")"
		if err := b.Send.EditScreen(ctx, u.ChatID, u.MessageID, edit); err != nil {
			b.Log.Error("editing a question", "err", err)
		}
	}
	b.sendAll(ctx, u.ChatID, reply.Send)
}

// rebuild moves the flow onto the members left.
func (b *Bot) rebuild(n *night) (flow.Screen, error) {
	n.rebuild = false
	taste, err := n.taste()
	if err != nil {
		return flow.Screen{}, err
	}
	return n.flow.Rebuild(taste)
}

// leaveNights takes a revoked user out of every session. A session that
// has started rebuilds at its next answer; one left with nobody ends.
func (b *Bot) leaveNights(ctx context.Context, user int64) {
	b.mu.Lock()
	var lobbies, ended []*night
	for chat, n := range b.nights {
		if n.member(user) == nil {
			continue
		}
		n.members = slices.DeleteFunc(n.members, func(m *member) bool { return m.id == user })
		delete(n.tastes, user)
		switch {
		case n.flow == nil:
			lobbies = append(lobbies, n)
		case len(n.members) == 0:
			delete(b.nights, chat)
			ended = append(ended, n)
		default:
			n.rebuild = true
		}
	}
	b.mu.Unlock()
	for _, n := range lobbies {
		if err := b.Send.EditScreen(ctx, n.chat, n.lobby, n.lobbyScreen()); err != nil {
			b.Log.Error("editing a lobby", "err", err)
		}
	}
	for _, n := range ended {
		if _, err := b.Send.Send(ctx, n.chat, MsgNightNoMembers); err != nil {
			b.Log.Error("ending a session", "err", err)
		}
	}
}

func (b *Bot) sendAll(ctx context.Context, chat int64, screens []flow.Screen) {
	for _, s := range screens {
		if _, err := b.Send.SendScreen(ctx, chat, s); err != nil {
			b.Log.Error("sending a screen", "err", err)
			return
		}
	}
}
