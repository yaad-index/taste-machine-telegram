// Package config reads the service's configuration from the environment
// (ADR 0001 section 6). Errors name the variable and never print its value.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Environment variable names.
const (
	EnvToken   = "TASTE_MACHINE_TELEGRAM_TOKEN"
	EnvAdmins  = "TASTE_MACHINE_TELEGRAM_ADMINS"
	EnvDataDir = "TASTE_MACHINE_TELEGRAM_DATA_DIR"
	EnvCompile = "TASTE_MACHINE_TELEGRAM_COMPILE"
)

// DefaultCompile is the engine's command, found on PATH unless EnvCompile
// names another.
const DefaultCompile = "taste-machine"

// Config is the service's configuration.
type Config struct {
	// Token is the bot token. It is a secret.
	Token string
	// Admins are the Telegram user ids that are always allowed and may run
	// the admin commands.
	Admins []int64
	// DataDir holds the allowlist file and every user's files.
	DataDir string
	// Compile is the engine's command, run as `<Compile> compile ...`.
	Compile string
}

// Load reads the configuration through getenv.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	c := Config{
		Token:   strings.TrimSpace(getenv(EnvToken)),
		DataDir: strings.TrimSpace(getenv(EnvDataDir)),
		Compile: strings.TrimSpace(getenv(EnvCompile)),
	}
	if c.Compile == "" {
		c.Compile = DefaultCompile
	}
	if c.Token == "" {
		errs = append(errs, fmt.Errorf("%s is not set", EnvToken))
	}
	if c.DataDir == "" {
		errs = append(errs, fmt.Errorf("%s is not set", EnvDataDir))
	}
	admins, err := parseIDs(getenv(EnvAdmins))
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvAdmins, err))
	}
	c.Admins = admins
	return c, errors.Join(errs...)
}

// parseIDs reads a comma-separated list of at least one user id.
func parseIDs(s string) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("not set; give one or more user ids, comma-separated")
	}
	var ids []int64
	for i, part := range strings.Split(s, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("entry %d is not a user id", i+1)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
