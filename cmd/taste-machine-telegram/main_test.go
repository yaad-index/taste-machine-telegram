package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "-version", "--version"} {
		var stdout, stderr bytes.Buffer
		assert.Equal(t, 0, run([]string{arg}, &stdout, &stderr), arg)
		assert.Equal(t, "taste-machine-telegram dev\n", stdout.String(), arg)
		assert.Empty(t, stderr.String(), arg)
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"version", "extra"}} {
		var stdout, stderr bytes.Buffer
		assert.Equal(t, 2, run(args, &stdout, &stderr), args)
		assert.Empty(t, stdout.String(), args)
		assert.Equal(t, "usage: taste-machine-telegram version\n", stderr.String(), args)
	}
}
