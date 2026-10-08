package flow_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/taste-machine/dataset"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/pick"
	"github.com/yaad-index/taste-machine/schema"
	"github.com/yaad-index/taste-machine/score"

	"github.com/yaad-index/taste-machine-telegram/flow"
)

// taste is a shelf of nine items over two askable fields, theme (a
// preference) and solo (a filter), with names; the member rated b highly.
func taste(t *testing.T, meta *fileformat.TasteMeta) score.Taste {
	t.Helper()
	sch := schema.Schema{Fields: []schema.Field{
		{Name: "theme", Type: schema.Category, Role: schema.Preference, Askable: true},
		{Name: "solo", Type: schema.Bool, Role: schema.Filter, Askable: true},
		{Name: "name", Type: schema.Category, Role: schema.Info, Display: true},
	}}
	shelf := &fileformat.Catalogue{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindCatalogue, Schema: sch}}
	for i, theme := range []string{"sea", "sea", "sea", "sea", "space", "space", "space", "space", "forest"} {
		id := string(rune('a' + i))
		shelf.Items = append(shelf.Items, fileformat.Item{ID: id, Facts: map[string]schema.Value{
			"theme": {Type: schema.Category, Category: theme},
			"solo":  {Type: schema.Bool, Bool: i == 0},
			"name":  {Type: schema.Category, Category: "Item " + strings.ToUpper(id)},
		}})
	}
	rating := 9.0
	low := 2.0
	tf := &fileformat.Taste{
		Meta:  fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindTaste, Taste: meta},
		Items: []fileformat.TasteItem{{ID: "b", Rating: &rating}, {ID: "f", Rating: &low}},
	}
	d, err := dataset.Load(shelf, []dataset.Input{{Taste: tf, Name: "me"}}, nil)
	require.NoError(t, err)
	return score.Learn(d, d.Members[0])
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
