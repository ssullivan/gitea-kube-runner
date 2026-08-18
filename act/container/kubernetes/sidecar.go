// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ container.ExecutionsEnvironment = (*sidecarView)(nil)

// sidecarView is one service container of a job Pod, presented as an
// ExecutionsEnvironment so the service orchestration in act/runner drives both backends
// unchanged. The lifecycle methods are no-ops because a sidecar is created, started and
// deleted with the Pod it belongs to, never on its own.
type sidecarView struct {
	container.LinuxContainerEnvironmentExtensions

	pod  *PodEnvironment
	name string
}

// NewSidecarView returns a view of one service container within the job's Pod.
func NewSidecarView(pod *PodEnvironment, name string) container.ExecutionsEnvironment {
	return &sidecarView{pod: pod, name: name}
}

func (s *sidecarView) Create(_, _ []string) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) Start(_ bool) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) Pull(_ bool) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) Remove() common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) Close() common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) ConnectToNetwork(_ string) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) Inspect(ctx context.Context) (*container.Info, error) {
	pod, err := s.pod.client.Clientset.CoreV1().Pods(s.pod.namespace).Get(ctx, s.pod.podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kubernetes: pod %s %w", s.pod.podName, container.ErrContainerNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("kubernetes: inspect pod %s: %w", s.pod.podName, err)
	}

	return podInfo(pod, s.name), nil
}

func (s *sidecarView) DumpLogs(ctx context.Context) error {
	return s.pod.dumpLogs(ctx, s.name)
}

func (s *sidecarView) Exec(command []string, env map[string]string, _, workdir string) common.Executor {
	return func(ctx context.Context) error {
		return s.pod.exec(ctx, execOptions{
			container: s.name,
			command:   command,
			env:       env,
			workdir:   workdir,
			stdout:    s.pod.stdout(),
			stderr:    s.pod.stderr(),
		})
	}
}

// The remaining methods only make sense for the job container, which is the only one
// the runner stages files into.

func (s *sidecarView) Copy(_ string, _ ...*container.FileEntry) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) CopyDir(_, _ string, _ bool) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) CopyTarStream(_ context.Context, _ string, _ io.Reader) error {
	return errors.New("kubernetes: copying into a service container is not supported")
}

func (s *sidecarView) GetContainerArchive(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, errors.New("kubernetes: reading files from a service container is not supported")
}

func (s *sidecarView) UpdateFromEnv(_ string, _ *map[string]string) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) UpdateFromImageEnv(_ *map[string]string) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (s *sidecarView) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	return s.pod.ReplaceLogWriter(stdout, stderr)
}
