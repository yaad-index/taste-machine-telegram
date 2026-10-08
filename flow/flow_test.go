package flow_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/taste-machine/dataset"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/group"
	"github.com/yaad-index/taste-machine/pick"
	"github.com/yaad-index/taste-machine/schema"
	"github.com/yaad-index/taste-machine/score"

	"github.com/yaad-index/taste-machine-telegram/flow"
)

// shelf is nine items over two askable fields, theme (a preference) and
// solo (a filter), with names.
func shelf() *fileformat.Catalogue {
	sch := schema.Schema{Fields: []schema.Field{
		{Name: "theme", Type: schema.Category, Role: schema.Preference, Askable: true},
		{Name: "solo", Type: schema.Bool, Role: schema.Filter, Askable: true},
		{Name: "name", Type: schema.Category, Role: schema.Info, Display: true},
	}}
	c := &fileformat.Catalogue{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindCatalogue, Schema: sch}}
	for i, theme := range []string{"sea", "sea", "sea", "sea", "space", "space", "space", "space", "forest"} {
		id := string(rune('a' + i))
		c.Items = append(c.Items, fileformat.Item{ID: id, Facts: map[string]schema.Value{
			"theme": {Type: schema.Category, Category: theme},
			"solo":  {Type: schema.Bool, Bool: i == 0},
			"name":  {Type: schema.Category, Category: "Item " + strings.ToUpper(id)},
		}})
	}
	return c
}

// tasteFile rates high highly and low poorly.
func tasteFile(meta *fileformat.TasteMeta, high, low string) *fileformat.Taste {
	h, l := 9.0, 2.0
	return &fileformat.Taste{
		Meta:  fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindTaste, Taste: meta},
		Items: []fileformat.TasteItem{{ID: high, Rating: &h}, {ID: low, Rating: &l}},
	}
}

// taste is one member who rated b highly and f poorly.
func taste(t *testing.T, meta *fileformat.TasteMeta) score.Taste {
	t.Helper()
	d, err := dataset.Load(shelf(), []dataset.Input{{Taste: tasteFile(meta, "b", "f"), Name: "me"}}, nil)
	require.NoError(t, err)
	return score.Learn(d, d.Members[0])
}

// pair is a group: ann as taste's member, and bob, who rated f highly.
func pair(t *testing.T) score.Taste {
	t.Helper()
	d, err := dataset.Load(shelf(), []dataset.Input{
		{Taste: tasteFile(nil, "b", "f"), Name: "ann"},
		{Taste: tasteFile(nil, "f", "b"), Name: "bob"},
	}, nil)
	require.NoError(t, err)
	g, err := group.New(d, 0, score.DefaultAnswerWeight)
	require.NoError(t, err)
	return g
}

// tap finds the button whose text starts with label on screen and taps it.
func tap(t *testing.T, s *flow.Session, screen flow.Screen, label string) flow.Reply {
	t.Helper()
	for _, row := range screen.Buttons {
		for _, b := range row {
			if strings.HasPrefix(b.Text, label) {
				r, err := s.Tap(b.Data)
				require.NoError(t, err)
				return r
			}
		}
	}
	t.Fatalf("no button %q on %q", label, screen.Text)
	return flow.Reply{}
}

func TestQuestionScreen(t *testing.T) {
	s := flow.New("abcd1234", taste(t, nil))
	q := s.Start()
	assert.Equal(t, "theme? (9 left)", q.Text)
	require.Len(t, q.Buttons, 5)
	assert.Equal(t, []flow.Button{{Text: "sea (4)", Data: "abcd1234 1 o0"}}, q.Buttons[0])
	assert.Equal(t, []flow.Button{{Text: "other", Data: "abcd1234 1 x"}, {Text: "no preference", Data: "abcd1234 1 n"}}, q.Buttons[3])
	assert.Equal(t, []flow.Button{{Text: "show results now", Data: "abcd1234 1 s"}}, q.Buttons[4])
	assert.Equal(t, "abcd1234", s.ID())
	for _, row := range q.Buttons {
		for _, b := range row {
			assert.LessOrEqual(t, len(b.Data), 64, "Telegram caps callback data at 64 bytes")
		}
	}
}

// The same answers through flow and straight through the engine give the
// same questions and the same ranked results.
func TestMatchesTheEngine(t *testing.T) {
	tt := taste(t, nil)
	s := flow.New("abcd1234", tt)
	engine := pick.New(tt)

	screen := s.Start()
	var asked []string
	for !s.Done() {
		q, ok := engine.Next()
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("%s? (%d left)", q.Field.Name, engine.Remaining()), screen.Text)
		asked = append(asked, q.Field.Name)
		_, err := engine.Apply(pick.Answer{Field: q.Field.Name, Kind: pick.Value, Key: q.Options[0].Key})
		require.NoError(t, err)
		r := tap(t, s, screen, q.Options[0].Label)
		require.NotNil(t, r.Edit)
		assert.Equal(t, q.Field.Name+": "+q.Options[0].Label, r.Edit.Text, "the question's message shows the answer")
		require.Len(t, r.Send, 1)
		screen = r.Send[0]
	}
	_, more := engine.Next()
	assert.False(t, more, "both stop together")
	assert.Equal(t, []string{"theme", "solo"}, asked)

	want := engine.Results()
	lines := []string{"Top picks:"}
	for i, r := range want[:min(flow.Top, len(want))] {
		lines = append(lines, fmt.Sprintf("%d. %s  %.3f", i+1, score.Label(r.ID, r.Name), r.Final))
		if pos := r.Positive(1); len(pos) > 0 {
			lines = append(lines, fmt.Sprintf("    %s = %s", pos[0].Field, pos[0].Value))
		}
	}
	assert.Equal(t, strings.Join(lines, "\n"), screen.Text, "the same ranking, in the same order")
}

func TestResultsShowNamesReasonsAndWhy(t *testing.T) {
	s := flow.New("abcd1234", taste(t, nil))
	screen := s.Start()
	r := tap(t, s, screen, "show results now")
	assert.Equal(t, &flow.Screen{Text: "theme: (stopped)"}, r.Edit)
	results := r.Send[0]
	assert.True(t, s.Done())
	assert.Contains(t, results.Text, "\n1. b  Item B  ", "the rated item leads")
	assert.Contains(t, results.Text, "\n    theme = sea", "the reason is the top positive contribution")
	require.Len(t, results.Buttons, 1)
	require.Len(t, results.Buttons[0], flow.Top)
	assert.Equal(t, flow.Button{Text: "why 1", Data: "abcd1234 w 0"}, results.Buttons[0][0])

	why := tap(t, s, results, "why 1")
	assert.Nil(t, why.Edit)
	require.Len(t, why.Send, 1)
	assert.True(t, strings.HasPrefix(why.Send[0].Text, "b  Item B  score "), why.Send[0].Text)
	assert.Contains(t, why.Send[0].Text, "+ theme = sea")

	_, err := s.Tap(screen.Buttons[0][0].Data)
	require.ErrorIs(t, err, flow.ErrStale, "the question was closed by stopping")
}

func TestStaleAndMalformedTaps(t *testing.T) {
	s := flow.New("abcd1234", taste(t, nil))
	first := s.Start()
	tap(t, s, first, "no preference")
	_, err := s.Tap(first.Buttons[0][0].Data)
	require.ErrorIs(t, err, flow.ErrStale, "a button of an answered question")
	for _, data := range []string{"", "abcd1234", "other 2 o0", "abcd1234 2 o9", "abcd1234 2 q", "abcd1234 2 ox", "abcd1234 w 0", "abcd1234 2 o0 extra"} {
		_, err := s.Tap(data)
		require.Error(t, err, data)
		assert.NotErrorIs(t, err, flow.ErrStale, data)
	}
}

func TestNoPreferenceSkipsTheField(t *testing.T) {
	s := flow.New("abcd1234", taste(t, nil))
	r := tap(t, s, s.Start(), "no preference")
	assert.Equal(t, "theme: no preference", r.Edit.Text)
	assert.Equal(t, "solo? (9 left)", r.Send[0].Text)
}

func TestOtherNarrowsToTheRest(t *testing.T) {
	s := flow.New("abcd1234", taste(t, nil))
	q := s.Start()
	var shown []string
	for _, row := range q.Buttons[:3] {
		label, _, _ := strings.Cut(row[0].Text, " (")
		shown = append(shown, label)
	}
	r := tap(t, s, q, "other")
	assert.Equal(t, "theme: other", r.Edit.Text)
	assert.Equal(t, "No items left:\n  9 removed: does not match the answer to theme (none of "+strings.Join(shown, ", ")+")", r.Send[0].Text,
		"every theme was shown, so other leaves nothing; the answer carries every shown key")
}

func TestEmptyAnswerOffersUndo(t *testing.T) {
	s := flow.New("abcd1234", taste(t, nil))
	r := tap(t, s, s.Start(), "no preference")
	solo := r.Send[0]
	require.Equal(t, "solo? (9 left)", solo.Text)
	r = tap(t, s, solo, "other")
	assert.Equal(t, "solo: other", r.Edit.Text)
	report := r.Send[0]
	assert.Equal(t, "No items left:\n  9 removed: does not match the answer to solo (none of false, true)", report.Text)
	assert.Equal(t, [][]flow.Button{{{Text: "Undo", Data: "abcd1234 3 u"}}}, report.Buttons)
	_, err := s.Tap(solo.Buttons[0][0].Data)
	require.ErrorIs(t, err, flow.ErrStale, "the question that emptied the list is closed")

	r = tap(t, s, report, "Undo")
	assert.Equal(t, "Undone; solo is skipped.", r.Edit.Text)
	assert.True(t, strings.HasPrefix(r.Send[0].Text, "Top picks:\n1. b  Item B  "), r.Send[0].Text)
	assert.True(t, s.Done())
}

func TestStartReportsDeclaredFilters(t *testing.T) {
	s := flow.New("abcd1234", taste(t, &fileformat.TasteMeta{Blocked: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}))
	screen := s.Start()
	assert.True(t, s.Done())
	assert.Equal(t, "Nothing on your shelf passes your filters:\n  9 removed: on the blocked list", screen.Text)
	assert.Empty(t, screen.Buttons)
}

func TestGroupResultsShowEachMember(t *testing.T) {
	s := flow.New("abcd1234", pair(t))
	r := tap(t, s, s.Start(), "show results now")
	assert.Regexp(t, `\n    ann -?\d\.\d{3}, bob -?\d\.\d{3}`, r.Send[0].Text, "each member's score, by label")
}

// A group that loses a member continues over the one left, and ends where
// a fresh single-member run with the same answers ends.
func TestRebuildMatchesAFreshRun(t *testing.T) {
	group := flow.New("abcd1234", pair(t))
	screen := group.Start()
	r := tap(t, group, screen, "sea")
	rebuilt, err := group.Rebuild(taste(t, nil))
	require.NoError(t, err)
	_, err = group.Tap(r.Send[0].Buttons[0][0].Data)
	require.ErrorIs(t, err, flow.ErrStale, "the screen shown before the rebuild is stale")

	fresh := flow.New("ffff0000", taste(t, nil))
	freshScreen := tap(t, fresh, fresh.Start(), "sea").Send[0]
	assert.Equal(t, freshScreen.Text, rebuilt.Text, "the same next question")

	got := tap(t, group, rebuilt, "show results now").Send[0].Text
	want := tap(t, fresh, freshScreen, "show results now").Send[0].Text
	assert.Equal(t, want, got)
	assert.NotContains(t, got, "bob", "no member scores in single mode")
}

func TestRebuildReplaysUndo(t *testing.T) {
	s := flow.New("abcd1234", pair(t))
	r := tap(t, s, s.Start(), "no preference")
	r = tap(t, s, r.Send[0], "other")
	require.True(t, strings.HasPrefix(r.Send[0].Text, "No items left:"))

	rebuilt, err := s.Rebuild(taste(t, nil))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rebuilt.Text, "No items left:"), "an empty report still awaiting Undo is shown again")
	r = tap(t, s, rebuilt, "Undo")
	assert.Equal(t, "Undone; solo is skipped.", r.Edit.Text)
	assert.True(t, strings.HasPrefix(r.Send[0].Text, "Top picks:\n1. b  Item B  "), "the Undo ran on the rebuilt flow: %s", r.Send[0].Text)

	again, err := s.Rebuild(pair(t))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(again.Text, "Top picks:"), "the Undo is replayed, not the empty report: %s", again.Text)
}

// Stopping for the results is replayed too: a flow that has just shown
// its results ranks them again over the new taste.
func TestRebuildAfterTheResults(t *testing.T) {
	s := flow.New("abcd1234", pair(t))
	tap(t, s, s.Start(), "show results now")
	got, err := s.Rebuild(taste(t, nil))
	require.NoError(t, err)
	assert.True(t, s.Done())

	fresh := flow.New("ffff0000", taste(t, nil))
	want := tap(t, fresh, fresh.Start(), "show results now").Send[0]
	assert.Equal(t, want.Text, got.Text)
	assert.Equal(t, flow.Button{Text: "why 1", Data: "abcd1234 w 0"}, got.Buttons[0][0])
}
