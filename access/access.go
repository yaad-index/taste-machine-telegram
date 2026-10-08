// Package access decides who may use the bot (ADR 0001 section 2): admins
// from the configuration, and users and group chats on an allowlist kept in
// one file, with one-time invites.
package access

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// InviteTTL is how long an invite link stays valid.
const InviteTTL = 24 * time.Hour

// ErrInvalidInvite is returned for a token that is unknown, used or expired.
var ErrInvalidInvite = errors.New("invalid invite")

// ErrAdmin is returned when an admin is revoked: admins come from the
// configuration, not the allowlist.
var ErrAdmin = errors.New("admins come from the configuration and cannot be revoked")

// file is the allowlist file's content. Invites hold the SHA-256 of each
// token, so the file never holds a usable link.
type file struct {
	Users   []int64  `json:"users"`
	Chats   []int64  `json:"chats"`
	Invites []invite `json:"invites"`
}

type invite struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

// Store is the allowlist. It is safe for concurrent use; every change is
// written to the file before it takes effect.
type Store struct {
	path   string
	admins map[int64]bool
	now    func() time.Time

	mu   sync.Mutex
	data file
}

// Open reads the allowlist file at path; a missing file is an empty list.
func Open(path string, admins []int64, now func() time.Time) (*Store, error) {
	s := &Store{path: path, admins: map[int64]bool{}, now: now}
	for _, id := range admins {
		s.admins[id] = true
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("allowlist %s: %w", path, err)
	}
	return s, nil
}

// IsAdmin reports whether id is an admin.
func (s *Store) IsAdmin(id int64) bool { return s.admins[id] }

// Admins lists the admin ids in ascending order.
func (s *Store) Admins() []int64 {
	out := make([]int64, 0, len(s.admins))
	for id := range s.admins {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// UserAllowed reports whether a user may use the bot.
func (s *Store) UserAllowed(id int64) bool {
	if s.admins[id] {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.data.Users, id)
}

// ChatAllowed reports whether the bot answers in a group chat.
func (s *Store) ChatAllowed(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.data.Chats, id)
}

// Invite creates a one-time token, valid for InviteTTL.
func (s *Store) Invite() (token string, expires time.Time, err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	expires = s.now().Add(InviteTTL)
	err = s.update(func(f *file) error {
		f.Invites = append(f.Invites, invite{Hash: hash(token), Expires: expires})
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Redeem consumes token and adds user to the allowlist. A token admits one
// user: once redeemed, or once expired, it returns ErrInvalidInvite.
func (s *Store) Redeem(token string, user int64) error {
	h := hash(token)
	return s.update(func(f *file) error {
		i := slices.IndexFunc(f.Invites, func(in invite) bool { return in.Hash == h })
		if i < 0 {
			return ErrInvalidInvite
		}
		f.Invites = slices.Delete(f.Invites, i, i+1)
		if !slices.Contains(f.Users, user) {
			f.Users = append(f.Users, user)
		}
		return nil
	})
}

// Revoke removes user from the allowlist. It reports whether the user was
// on it.
func (s *Store) Revoke(user int64) (bool, error) {
	if s.admins[user] {
		return false, ErrAdmin
	}
	found := false
	err := s.update(func(f *file) error {
		i := slices.Index(f.Users, user)
		if i < 0 {
			return nil
		}
		found = true
		f.Users = slices.Delete(f.Users, i, i+1)
		return nil
	})
	return found, err
}

// Allow adds a group chat.
func (s *Store) Allow(chat int64) error {
	return s.update(func(f *file) error {
		if !slices.Contains(f.Chats, chat) {
			f.Chats = append(f.Chats, chat)
		}
		return nil
	})
}

// Disallow removes a group chat.
func (s *Store) Disallow(chat int64) error {
	return s.update(func(f *file) error {
		f.Chats = slices.DeleteFunc(f.Chats, func(c int64) bool { return c == chat })
		return nil
	})
}

// update applies change to a copy of the list with expired invites pruned,
// writes the copy, and only then makes it current. A failed change or
// write leaves the list as it was.
func (s *Store) update(change func(*file) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	next := file{
		Users: slices.Clone(s.data.Users),
		Chats: slices.Clone(s.data.Chats),
		Invites: slices.DeleteFunc(slices.Clone(s.data.Invites), func(in invite) bool {
			return !now.Before(in.Expires)
		}),
	}
	if err := change(&next); err != nil {
		return err
	}
	slices.Sort(next.Users)
	slices.Sort(next.Chats)
	if err := writeFile(s.path, next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// writeFile writes f to a temporary file next to path and renames it into
// place, so a reader never sees a half-written list.
func writeFile(path string, f file) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".allowlist-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
