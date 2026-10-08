// Package flow turns the engine's question flow (engine ADR 0002 sections
// 6 and 7) into chat screens: a text with buttons, and the edits and new
// screens a tap on a button leads to (ADR 0001 section 4). It knows nothing
// of Telegram.
package flow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/yaad-index/taste-machine/pick"
	"github.com/yaad-index/taste-machine/score"
)

// Top is how many results the results screen lists.
const Top = 5

// Button is one button. Data comes back when it is tapped.
type Button struct {
	Text string
	Data string
}

// Screen is a message: text and rows of buttons.
type Screen struct {
	Text    string
	Buttons [][]Button
}

// Reply is what a tap leads to: an edit of the tapped message (nil leaves
// it alone), then new screens to send.
type Reply struct {
	Edit *Screen
	Send []Screen
}

// ErrStale is returned for a tap on a button of a question already
// answered.
var ErrStale = errors.New("that question was already answered")

// Session is one run of the question flow.
type Session struct {
	id    string
	taste score.Taste
	pick  *pick.Session
	// step counts the screens with answer buttons; a tap must carry the
	// current step.
	step     int
	question pick.Question
	results  []score.Result
	done     bool
	// log is every answer applied, with Undo as a step of its own, so the
	// flow can be replayed over another taste.
	log []step
}

// step is one entry of the log: an answer, an Undo of the one before, or
// stopping for the results.
type step struct {
	answer pick.Answer
	undo   bool
	stop   bool
}

// New starts a session with the given id over a taste.
func New(id string, taste score.Taste) *Session {
	return &Session{id: id, taste: taste, pick: pick.New(taste)}
}

// ID returns the session's id, the prefix of its buttons' data.
func (s *Session) ID() string { return s.id }

// Done reports whether the results have been shown.
func (s *Session) Done() bool { return s.done }

// Start returns the first screen: the first question, the report when the
// declared filters leave nothing, or the results when nothing needs asking.
func (s *Session) Start() Screen {
	if r := s.pick.Empty(); r != nil {
		s.done = true
		return Screen{Text: report("Nothing on your shelf passes your filters:", *r)}
	}
	return s.next("")
}

// next returns the next question, or the results when there is none.
// prefix is put before the question's text.
func (s *Session) next(prefix string) Screen {
	q, ok := s.pick.Next()
	if !ok {
		return s.finish(prefix)
	}
	s.step++
	s.question = q
	var rows [][]Button
	for i, o := range q.Options {
		rows = append(rows, []Button{{Text: fmt.Sprintf("%s (%d)", o.Label, o.Count), Data: s.data("o" + strconv.Itoa(i))}})
	}
	rows = append(rows,
		[]Button{{Text: "other", Data: s.data("x")}, {Text: "no preference", Data: s.data("n")}},
		[]Button{{Text: "show results now", Data: s.data("s")}})
	return Screen{Text: prefix + fmt.Sprintf("%s? (%d left)", q.Field.Name, s.pick.Remaining()), Buttons: rows}
}

// finish ranks what remains and returns the results screen.
func (s *Session) finish(prefix string) Screen {
	s.done = true
	s.step++
	s.results = s.pick.Results()
	if len(s.results) == 0 {
		return Screen{Text: prefix + "Your shelf is empty."}
	}
	var b strings.Builder
	b.WriteString(prefix + "Top picks:")
	var buttons []Button
	for i, r := range s.results[:min(Top, len(s.results))] {
		fmt.Fprintf(&b, "\n%d. %s  %.3f", i+1, score.Label(r.ID, r.Name), r.Final)
		if pos := r.Positive(1); len(pos) > 0 {
			fmt.Fprintf(&b, "\n    %s = %s", pos[0].Field, pos[0].Value)
		}
		if len(r.Members) > 0 {
			parts := make([]string, 0, len(r.Members))
			for _, m := range r.Members {
				parts = append(parts, fmt.Sprintf("%s %.3f", m.Label, m.Score))
			}
			b.WriteString("\n    " + strings.Join(parts, ", "))
		}
		buttons = append(buttons, Button{Text: fmt.Sprintf("why %d", i+1), Data: s.id + " w " + strconv.Itoa(i)})
	}
	return Screen{Text: b.String(), Buttons: [][]Button{buttons}}
}

// Tap handles a tap on one of the session's buttons.
func (s *Session) Tap(data string) (Reply, error) {
	parts := strings.Fields(data)
	if len(parts) != 3 || parts[0] != s.id {
		return Reply{}, fmt.Errorf("malformed button data %q", data)
	}
	if parts[1] == "w" {
		return s.why(parts[2])
	}
	// Showing the results takes a step too, so after them every question
	// button is stale.
	if parts[1] != strconv.Itoa(s.step) {
		return Reply{}, ErrStale
	}
	act := parts[2]
	f := s.question.Field
	switch {
	case act == "s":
		s.log = append(s.log, step{stop: true})
		return Reply{Edit: &Screen{Text: f.Name + ": (stopped)"}, Send: []Screen{s.finish("")}}, nil
	case act == "u":
		s.pick.Undo()
		s.log = append(s.log, step{undo: true})
		return Reply{Edit: &Screen{Text: "Undone; " + f.Name + " is skipped."}, Send: []Screen{s.next("")}}, nil
	case act == "n":
		return s.apply(pick.Answer{Field: f.Name, Kind: pick.NoPreference}, "no preference")
	case act == "x":
		shown := make([]string, 0, len(s.question.Options))
		for _, o := range s.question.Options {
			shown = append(shown, o.Key)
		}
		return s.apply(pick.Answer{Field: f.Name, Kind: pick.Other, Shown: shown}, "other")
	case strings.HasPrefix(act, "o"):
		i, err := strconv.Atoi(act[1:])
		if err != nil || i < 0 || i >= len(s.question.Options) {
			return Reply{}, fmt.Errorf("malformed button data %q", data)
		}
		o := s.question.Options[i]
		return s.apply(pick.Answer{Field: f.Name, Kind: pick.Value, Key: o.Key}, o.Label)
	}
	return Reply{}, fmt.Errorf("malformed button data %q", data)
}

// apply answers the current question. The question's message is edited to
// show the answer; an answer that leaves nothing gets the engine's report
// with an Undo button.
func (s *Session) apply(a pick.Answer, label string) (Reply, error) {
	out, err := s.pick.Apply(a)
	if err != nil {
		return Reply{}, err
	}
	s.log = append(s.log, step{answer: a})
	edit := &Screen{Text: a.Field + ": " + label}
	if out.Empty != nil {
		return Reply{Edit: edit, Send: []Screen{s.emptyScreen(*out.Empty)}}, nil
	}
	// Options come from the items that remain, each with at least one
	// match, so an answer from a button is never unmet.
	return Reply{Edit: edit, Send: []Screen{s.next("")}}, nil
}

func (s *Session) emptyScreen(r score.EmptyReport) Screen {
	s.step++
	return Screen{
		Text:    report("No items left:", r),
		Buttons: [][]Button{{{Text: "Undo", Data: s.data("u")}}},
	}
}

// Rebuild moves the flow onto another taste, as when a member leaves a
// group: the answers given so far are applied again, in order, and the
// screen to show next is returned, replacing the one shown before, whose
// buttons go stale. When the flow has just finished, its results are
// ranked again over the new taste.
func (s *Session) Rebuild(taste score.Taste) (Screen, error) {
	p := pick.New(taste)
	var last pick.Outcome
	stopped := false
	for _, st := range s.log {
		switch {
		case st.stop:
			stopped = true
			continue
		case st.undo:
			p.Undo()
			last = pick.Outcome{}
			continue
		}
		out, err := p.Apply(st.answer)
		if err != nil {
			return Screen{}, err
		}
		last = out
	}
	s.taste, s.pick, s.done = taste, p, false
	switch {
	case stopped:
		return s.finish(""), nil
	case last.Empty != nil:
		return s.emptyScreen(*last.Empty), nil
	}
	return s.next(""), nil
}

// why returns a result's full explanation as a new screen.
func (s *Session) why(arg string) (Reply, error) {
	i, err := strconv.Atoi(arg)
	if err != nil || i < 0 || i >= min(Top, len(s.results)) {
		return Reply{}, fmt.Errorf("malformed button data %q", arg)
	}
	return Reply{Send: []Screen{{Text: s.results[i].Explain()}}}, nil
}

func (s *Session) data(act string) string {
	return s.id + " " + strconv.Itoa(s.step) + " " + act
}

func report(head string, r score.EmptyReport) string {
	var b strings.Builder
	b.WriteString(head)
	for _, c := range r.Causes {
		fmt.Fprintf(&b, "\n  %d removed: %s", c.Items, c.Cause)
	}
	return b.String()
}
