package access_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/taste-machine-telegram/access"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func open(t *testing.T, dir string, c *clock) *access.Store {
	t.Helper()
	s, err := access.Open(filepath.Join(dir, "allowlist.json"), []int64{1}, c.now)
	require.NoError(t, err)
	return s
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Unix(0, 0)})
	assert.True(t, s.UserAllowed(1), "an admin is allowed without being on the list")
	assert.True(t, s.IsAdmin(1))
	assert.False(t, s.UserAllowed(2))
	assert.False(t, s.ChatAllowed(-5))
	assert.Equal(t, []int64{1}, s.Admins())
}

func TestOpenRejectsACorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "allowlist.json")
	require.NoError(t, os.WriteFile(p, []byte("{"), 0o600))
	_, err := access.Open(p, nil, time.Now)
	assert.ErrorContains(t, err, "allowlist")
}

func TestInviteAdmitsOneUserAndPersists(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Unix(1000, 0)}
	s := open(t, dir, c)
	token, expires, err := s.Invite()
	require.NoError(t, err)
	assert.Len(t, token, 22, "16 random bytes, base64url")
	assert.Equal(t, c.t.Add(access.InviteTTL), expires)

	b, err := os.ReadFile(filepath.Join(dir, "allowlist.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(b), token, "the file holds only the token's hash")

	require.NoError(t, s.Redeem(token, 7))
	assert.True(t, s.UserAllowed(7))
	require.ErrorIs(t, s.Redeem(token, 8), access.ErrInvalidInvite, "a token admits one user")
	assert.False(t, s.UserAllowed(8))
	require.ErrorIs(t, s.Redeem("unknown", 8), access.ErrInvalidInvite)

	again := open(t, dir, c)
	assert.True(t, again.UserAllowed(7), "the list survives a restart")
	require.ErrorIs(t, again.Redeem(token, 8), access.ErrInvalidInvite, "and so does the token being used")
}

func TestConcurrentRedeemAdmitsExactlyOne(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Unix(1000, 0)})
	token, _, err := s.Invite()
	require.NoError(t, err)
	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Redeem(token, int64(100+i))
		}()
	}
	wg.Wait()
	admitted := 0
	for i, err := range errs {
		if err == nil {
			admitted++
			assert.True(t, s.UserAllowed(int64(100+i)))
		} else {
			require.ErrorIs(t, err, access.ErrInvalidInvite)
			assert.False(t, s.UserAllowed(int64(100+i)))
		}
	}
	assert.Equal(t, 1, admitted)
}

func TestInviteExpires(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Unix(1000, 0)}
	s := open(t, dir, c)
	early, _, err := s.Invite()
	require.NoError(t, err)
	late, _, err := s.Invite()
	require.NoError(t, err)

	c.t = c.t.Add(access.InviteTTL - time.Second)
	require.NoError(t, s.Redeem(early, 7), "valid until the last second")

	c.t = c.t.Add(time.Second)
	require.ErrorIs(t, s.Redeem(late, 8), access.ErrInvalidInvite, "expired at InviteTTL")

	require.NoError(t, s.Allow(-5))
	b, err := os.ReadFile(filepath.Join(dir, "allowlist.json"))
	require.NoError(t, err)
	assert.Contains(t, string(b), `"invites": []`, "expired invites are pruned by the next change")
}

func TestRevoke(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Unix(1000, 0)})
	token, _, err := s.Invite()
	require.NoError(t, err)
	require.NoError(t, s.Redeem(token, 7))

	found, err := s.Revoke(7)
	require.NoError(t, err)
	assert.True(t, found)
	assert.False(t, s.UserAllowed(7))

	found, err = s.Revoke(7)
	require.NoError(t, err)
	assert.False(t, found)

	_, err = s.Revoke(1)
	require.ErrorIs(t, err, access.ErrAdmin)
	assert.True(t, s.UserAllowed(1))
}

func TestAllowAndDisallowChats(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Unix(1000, 0)}
	s := open(t, dir, c)
	require.NoError(t, s.Allow(-5))
	require.NoError(t, s.Allow(-5))
	require.NoError(t, s.Allow(-6))
	assert.True(t, s.ChatAllowed(-5))
	require.NoError(t, s.Disallow(-5))
	assert.False(t, s.ChatAllowed(-5))
	again := open(t, dir, c)
	assert.True(t, again.ChatAllowed(-6))
	assert.False(t, again.ChatAllowed(-5))
}

func TestFailedWriteLeavesTheListUnchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, os.Mkdir(dir, 0o700))
	s := open(t, dir, &clock{time.Unix(1000, 0)})
	require.NoError(t, os.Remove(dir))
	require.Error(t, s.Allow(-5))
	assert.False(t, s.ChatAllowed(-5))
	_, _, err := s.Invite()
	require.Error(t, err)
}
