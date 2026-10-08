// Package userfiles keeps each user's shelf and taste files in the data
// directory (ADR 0001 sections 1 and 3). A compile writes into a new
// version directory, and a "current" symlink is swapped to it by rename,
// so a reader sees the old pair or the new pair, never a mix.
package userfiles

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yaad-index/taste-machine/fileformat"
)

// File names inside a version directory.
const (
	ShelfFile = "shelf.zip"
	TasteFile = "taste.zip"
	LinkFile  = "link.json"
	current   = "current"
)

// ErrNotLinked is returned for a user with no files.
var ErrNotLinked = errors.New("not linked")

// Link is the source account a user's files were compiled from.
type Link struct {
	Source string `json:"source"`
	User   string `json:"user"`
}

// Summary describes a committed pair of files.
type Summary struct {
	Shelf int
	Rated int
}

// Store is the users directory. Commit and Remove are serialised, so a
// removal cannot interleave with a swap.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New returns the store under dataDir.
func New(dataDir string) *Store { return &Store{dir: filepath.Join(dataDir, "users")} }

// Dir is the user's directory.
func (s *Store) Dir(user int64) string { return filepath.Join(s.dir, strconv.FormatInt(user, 10)) }

// Paths returns the user's current shelf and taste files. The link is
// resolved once, so both paths name the same version.
func (s *Store) Paths(user int64) (shelf, taste string, err error) {
	target, err := os.Readlink(filepath.Join(s.Dir(user), current))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", ErrNotLinked
	}
	if err != nil {
		return "", "", err
	}
	v := filepath.Join(s.Dir(user), target)
	return filepath.Join(v, ShelfFile), filepath.Join(v, TasteFile), nil
}

// Link returns the account the user's current files came from.
func (s *Store) Link(user int64) (Link, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir(user), current, LinkFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Link{}, ErrNotLinked
	}
	if err != nil {
		return Link{}, err
	}
	var l Link
	if err := json.Unmarshal(b, &l); err != nil {
		return Link{}, fmt.Errorf("link file: %w", err)
	}
	return l, nil
}

// LastCompile is the modification time of the user's current taste file:
// the time of the last successful compile. ok is false for a user with no
// files.
func (s *Store) LastCompile(user int64) (t time.Time, ok bool) {
	fi, err := os.Stat(filepath.Join(s.Dir(user), current, TasteFile))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// Stage creates an empty version directory for a compile to write into.
func (s *Store) Stage(user int64) (string, error) {
	if err := os.MkdirAll(s.Dir(user), 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(s.Dir(user), "v-")
}

// Commit checks that staged holds a readable shelf and taste file, records
// the link, and makes it the user's current version. The version before it
// is kept, so a reader that resolved it just before the swap can still
// read it; older ones are removed. keep is asked under the store's lock, and when it says no
// (the user was revoked meanwhile) the staged files are discarded. On any
// failure staged is removed and the current version is left as it was.
func (s *Store) Commit(user int64, staged string, link Link, keep func() bool) (sum Summary, err error) {
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staged)
		}
	}()
	shelf, err := fileformat.ReadCatalogueFile(filepath.Join(staged, ShelfFile))
	if err != nil {
		return Summary{}, err
	}
	taste, err := fileformat.ReadTasteFile(filepath.Join(staged, TasteFile))
	if err != nil {
		return Summary{}, err
	}
	sum.Shelf = len(shelf.Items)
	for _, it := range taste.Items {
		if it.Rating != nil {
			sum.Rated++
		}
	}
	b, err := json.Marshal(link)
	if err != nil {
		return Summary{}, err
	}
	if err := os.WriteFile(filepath.Join(staged, LinkFile), b, 0o600); err != nil {
		return Summary{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !keep() {
		return Summary{}, errors.New("the user is no longer allowed")
	}
	cur := filepath.Join(s.Dir(user), current)
	previous, _ := os.Readlink(cur)
	tmp := filepath.Join(s.Dir(user), "current-"+random())
	if err := os.Symlink(filepath.Base(staged), tmp); err != nil {
		return Summary{}, err
	}
	if err := os.Rename(tmp, cur); err != nil {
		_ = os.Remove(tmp)
		return Summary{}, err
	}
	entries, err := os.ReadDir(s.Dir(user))
	if err != nil {
		return sum, nil
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "v-") && name != filepath.Base(staged) && name != previous {
			_ = os.RemoveAll(filepath.Join(s.Dir(user), name))
		}
	}
	return sum, nil
}

// Discard removes a staged directory that will not be committed.
func (s *Store) Discard(staged string) { _ = os.RemoveAll(staged) }

// Remove deletes all of the user's files.
func (s *Store) Remove(user int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.Dir(user))
}

func random() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
