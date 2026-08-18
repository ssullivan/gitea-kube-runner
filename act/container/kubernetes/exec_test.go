// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapCommand(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		env     map[string]string
		workdir string
		want    []string
	}{
		{
			name:    "bare command is passed through",
			command: []string{"echo", "hi"},
			want:    []string{"echo", "hi"},
		},
		{
			// The exec subresource has no env parameter, so `env` supplies it.
			name:    "env is applied with env(1), sorted for determinism",
			command: []string{"echo", "hi"},
			env:     map[string]string{"B": "2", "A": "1"},
			want:    []string{"env", "A=1", "B=2", "echo", "hi"},
		},
		{
			// $0 is the workdir and $@ the command, so the command needs no quoting.
			name:    "workdir is applied with a shell wrapper",
			command: []string{"echo", "hi"},
			workdir: "/workspace",
			want:    []string{"sh", "-c", `cd "$0" && exec "$@"`, "/workspace", "echo", "hi"},
		},
		{
			name:    "env and workdir compose",
			command: []string{"echo", "hi"},
			env:     map[string]string{"A": "1"},
			workdir: "/workspace",
			want:    []string{"sh", "-c", `cd "$0" && exec "$@"`, "/workspace", "env", "A=1", "echo", "hi"},
		},
		{
			name:    "a value with spaces survives as one argument",
			command: []string{"sh", "-c", "echo $MSG"},
			env:     map[string]string{"MSG": "hello world"},
			want:    []string{"env", "MSG=hello world", "sh", "-c", "echo $MSG"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, wrapCommand(tt.command, tt.env, tt.workdir))
		})
	}
}

func TestDrainedPipeWriterSurvivesAConsumerThatStopsReading(t *testing.T) {
	reader, writer := io.Pipe()
	w := drainedPipeWriter{writer}
	require.NoError(t, reader.Close())

	// io.Copy must not see an error, or client-go logs one for every early-closed read.
	n, err := w.Write([]byte("discarded"))
	require.NoError(t, err)
	require.Equal(t, len("discarded"), n)
}
