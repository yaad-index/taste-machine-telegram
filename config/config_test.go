package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/taste-machine-telegram/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoad(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		config.EnvToken:   " secret-token ",
		config.EnvAdmins:  "11, 22",
		config.EnvDataDir: "/data",
	}))
	require.NoError(t, err)
	assert.Equal(t, "secret-token", c.Token)
	assert.Equal(t, []int64{11, 22}, c.Admins)
	assert.Equal(t, "/data", c.DataDir)
	assert.Equal(t, config.DefaultCompile, c.Compile)
	assert.Equal(t, config.DefaultHealth, c.Health)
	assert.Empty(t, c.APIURL, "Telegram's own server")

	c, err = config.Load(env(map[string]string{config.EnvToken: "t", config.EnvAdmins: "1", config.EnvDataDir: "/d", config.EnvCompile: "/opt/tm", config.EnvHealth: "127.0.0.1:9", config.EnvAPIURL: "http://api.local"}))
	require.NoError(t, err)
	assert.Equal(t, "/opt/tm", c.Compile)
	assert.Equal(t, "127.0.0.1:9", c.Health)
	assert.Equal(t, "http://api.local", c.APIURL)
}

func TestLoadErrors(t *testing.T) {
	_, err := config.Load(env(nil))
	require.Error(t, err)
	assert.ErrorContains(t, err, config.EnvToken+" is not set")
	assert.ErrorContains(t, err, config.EnvDataDir+" is not set")
	assert.ErrorContains(t, err, config.EnvAdmins+": not set")

	for _, admins := range []string{"11,x", "11,", "11,-3", "0"} {
		_, err = config.Load(env(map[string]string{config.EnvToken: "secret-token", config.EnvAdmins: admins, config.EnvDataDir: "/d"}))
		require.Error(t, err, admins)
		assert.ErrorContains(t, err, "is not a user id", admins)
		assert.NotContains(t, err.Error(), "secret-token", "an error never prints the token")
	}
}
