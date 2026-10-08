package userfiles_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/taste-machine/fileformat"
	"github.com/yaad-index/taste-machine/schema"

	"github.com/yaad-index/taste-machine-telegram/userfiles"
)

// writePair writes a shelf of n items and a taste file rating the first
// rated of them into dir.
func writePair(t *testing.T, dir string, n, rated int) {
	t.Helper()
	s := schema.Schema{Fields: []schema.Field{{Name: "theme", Type: schema.Category, Role: schema.Preference}}}
	shelf := &fileformat.Catalogue{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindCatalogue, Schema: s}}
	taste := &fileformat.Taste{Meta: fileformat.Metadata{FormatVersion: 1, SchemaID: "s", Kind: fileformat.KindTaste}}
	for i := range n {
		id := fmt.Sprintf("i%d", i)
		shelf.Items = append(shelf.Items, fileformat.Item{ID: id, Facts: map[string]schema.Value{"theme": {Type: schema.Category, Category: "sea"}}})
		item := fileformat.TasteItem{ID: id, Plays: 1}
		if i < rated {
			r := 5.0
			item.Rating = &r
		}
		taste.Items = append(taste.Items, item)
	}
	require.NoError(t, fileformat.WriteFile(filepath.Join(dir, userfiles.ShelfFile), shelf))
	require.NoError(t, fileformat.WriteFile(filepath.Join(dir, userfiles.TasteFile), taste))
}

func always() bool { return true }

func commit(t *testing.T, s *userfiles.Store, user int64, n, rated int, link userfiles.Link) userfiles.Summary {
	t.Helper()
	staged, err := s.Stage(user)
	require.NoError(t, err)
	writePair(t, staged, n, rated)
	sum, err := s.Commit(user, staged, link, always)
	require.NoError(t, err)
	return sum
}

func versions(t *testing.T, s *userfiles.Store, user int64) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(s.Dir(user), "v-*"))
	require.NoError(t, err)
	return m
}

func TestNotLinked(t *testing.T) {
	s := userfiles.New(t.TempDir())
	_, _, err := s.Paths(7)
	require.ErrorIs(t, err, userfiles.ErrNotLinked)
	_, err = s.Link(7)
	require.ErrorIs(t, err, userfiles.ErrNotLinked)
	_, ok := s.LastCompile(7)
	assert.False(t, ok)
}

func TestCommit(t *testing.T) {
	s := userfiles.New(t.TempDir())
	link := userfiles.Link{Source: "src", User: "someone"}
	before := time.Now().Add(-time.Second)
	sum := commit(t, s, 7, 4, 3, link)
	assert.Equal(t, userfiles.Summary{Shelf: 4, Rated: 3}, sum)

	got, err := s.Link(7)
	require.NoError(t, err)
	assert.Equal(t, link, got)
	when, ok := s.LastCompile(7)
	require.True(t, ok)
	assert.True(t, when.After(before))

	shelf, taste, err := s.Paths(7)
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(shelf), filepath.Dir(taste))
	c, err := fileformat.ReadCatalogueFile(shelf)
	require.NoError(t, err)
	assert.Len(t, c.Items, 4)
}

func TestCommitKeepsOnePreviousVersion(t *testing.T) {
	s := userfiles.New(t.TempDir())
	commit(t, s, 7, 1, 0, userfiles.Link{Source: "src", User: "a"})
	first, _, err := s.Paths(7)
	require.NoError(t, err)
	commit(t, s, 7, 2, 0, userfiles.Link{Source: "src", User: "b"})
	assert.FileExists(t, first, "the version before the current one is kept")
	commit(t, s, 7, 3, 0, userfiles.Link{Source: "src", User: "c"})
	assert.NoFileExists(t, first, "older versions are removed")
	assert.Len(t, versions(t, s, 7), 2)

	got, err := s.Link(7)
	require.NoError(t, err)
	assert.Equal(t, "c", got.User, "a new link replaces the old one")
}

func TestFailedCommitLeavesCurrentAlone(t *testing.T) {
	s := userfiles.New(t.TempDir())
	commit(t, s, 7, 2, 1, userfiles.Link{Source: "src", User: "a"})
	shelf, _, err := s.Paths(7)
	require.NoError(t, err)

	staged, err := s.Stage(7)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staged, userfiles.ShelfFile), []byte("not a zip"), 0o600))
	_, err = s.Commit(7, staged, userfiles.Link{Source: "src", User: "b"}, always)
	require.Error(t, err)
	assert.NoDirExists(t, staged, "the staged directory is removed")
	after, _, err := s.Paths(7)
	require.NoError(t, err)
	assert.Equal(t, shelf, after)
	got, err := s.Link(7)
	require.NoError(t, err)
	assert.Equal(t, "a", got.User)
}

func TestCommitDiscardsWhenNoLongerKept(t *testing.T) {
	s := userfiles.New(t.TempDir())
	staged, err := s.Stage(7)
	require.NoError(t, err)
	writePair(t, staged, 1, 0)
	_, err = s.Commit(7, staged, userfiles.Link{Source: "src", User: "a"}, func() bool { return false })
	require.Error(t, err)
	assert.NoDirExists(t, staged)
	_, _, err = s.Paths(7)
	require.ErrorIs(t, err, userfiles.ErrNotLinked)
}

func TestRemove(t *testing.T) {
	s := userfiles.New(t.TempDir())
	commit(t, s, 7, 1, 0, userfiles.Link{Source: "src", User: "a"})
	require.NoError(t, s.Remove(7))
	assert.NoDirExists(t, s.Dir(7))
	require.NoError(t, s.Remove(7), "removing nothing is fine")
}

// A reader racing commits never gets a shelf and a taste file from
// different versions: each version has n shelf items and n taste items.
func TestReaderNeverSeesAMixedPair(t *testing.T) {
	s := userfiles.New(t.TempDir())
	commit(t, s, 7, 1, 0, userfiles.Link{Source: "src", User: "a"})
	var stop atomic.Bool
	var matched atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			shelfPath, tastePath, err := s.Paths(7)
			if err != nil {
				continue
			}
			shelf, err1 := fileformat.ReadCatalogueFile(shelfPath)
			taste, err2 := fileformat.ReadTasteFile(tastePath)
			if err1 != nil || err2 != nil {
				continue // the version was removed two swaps later
			}
			assert.Len(t, taste.Items, len(shelf.Items), "a mixed pair")
			matched.Add(1)
		}
	}()
	for n := 2; n <= 30; n++ {
		commit(t, s, 7, n, 0, userfiles.Link{Source: "src", User: "a"})
	}
	stop.Store(true)
	wg.Wait()
	assert.Positive(t, matched.Load(), "the reader read some pairs")
}
