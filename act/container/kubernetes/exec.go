// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
)

// execOptions is one exec call against a container in the job's Pod.
type execOptions struct {
	container string
	command   []string
	env       map[string]string
	workdir   string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	tty       bool
}

// exec runs a command in the Pod over the exec subresource. A non-zero exit is
// reported as container.ExitCodeError so callers treat it like a docker exec.
func (e *PodEnvironment) exec(ctx context.Context, opts execOptions) error {
	if e.podName == "" {
		return errors.New("kubernetes: the job pod has not been created yet")
	}

	request := e.client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(e.podName).
		Namespace(e.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: opts.container,
			Command:   wrapCommand(opts.command, opts.env, opts.workdir),
			Stdin:     opts.stdin != nil,
			Stdout:    opts.stdout != nil,
			Stderr:    opts.stderr != nil,
			TTY:       opts.tty,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.client.RESTMap, "POST", request.URL())
	if err != nil {
		return fmt.Errorf("kubernetes: build exec request: %w", err)
	}

	// The exec subresource delivers stdout and stderr as two streams copied by
	// concurrent goroutines, where docker delivers one stream this backend would
	// demultiplex itself. Callers routinely pass the same log writer for both, so the
	// writes have to be serialized or they race.
	var mu sync.Mutex
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  opts.stdin,
		Stdout: syncWriter(&mu, opts.stdout),
		Stderr: syncWriter(&mu, opts.stderr),
		Tty:    opts.tty,
	})

	// Flush any buffered, not-yet-newline-terminated trailing line, as the docker
	// backend does, so a final unterminated line is not lost.
	common.FlushWriter(opts.stdout)
	common.FlushWriter(opts.stderr)

	var codeErr k8sexec.CodeExitError
	if errors.As(err, &codeErr) {
		return container.ExitCodeError(codeErr.Code)
	}
	if err != nil {
		return fmt.Errorf("kubernetes: exec in pod %s: %w", e.podName, err)
	}

	return nil
}

// syncWriter guards w with mu, so the two exec streams can share a writer. A nil
// writer stays nil, which is what tells the exec request not to request that stream.
func syncWriter(mu *sync.Mutex, w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &lockedWriter{mu: mu, w: w}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// wrapCommand applies the env and working directory the exec subresource has no
// parameters for. `env` and `sh` are the most portable way to do it, and are present
// in busybox as well as coreutils images.
func wrapCommand(command []string, env map[string]string, workdir string) []string {
	cmd := command

	if len(env) > 0 {
		envCmd := []string{"env"}
		for _, name := range slices.Sorted(maps.Keys(env)) {
			envCmd = append(envCmd, name+"="+env[name])
		}
		cmd = append(envCmd, cmd...)
	}

	if workdir != "" {
		// `sh -c script $0 $@`: the script cds to $0 then execs the rest, so no
		// quoting of the command itself is needed.
		cmd = append([]string{"sh", "-c", `cd "$0" && exec "$@"`, workdir}, cmd...)
	}

	return cmd
}
