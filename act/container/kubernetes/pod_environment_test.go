// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"runtime"
	"testing"

	"gitea.com/gitea/runner/act/container"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWith(phase corev1.PodPhase, statuses []corev1.ContainerStatus, spec []corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "abc-123"},
		Spec:       corev1.PodSpec{Containers: spec},
		Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: statuses},
	}
}

func TestRunnerContextAnswersArchWithoutADockerDaemon(t *testing.T) {
	// The shared Linux extensions resolve arch through the docker daemon, which a job pod's
	// cluster has none of: runner.arch came back empty and every step warned into the job log.
	ctx := (&PodEnvironment{input: &PodInput{}}).GetRunnerContext(context.Background())

	assert.Equal(t, "Linux", ctx["os"])
	assert.Equal(t, container.DefaultToolCache, ctx["tool_cache"])
	assert.NotEmpty(t, ctx["arch"], "an empty runner.arch is what this exists to prevent")
	assert.Contains(t, []string{"X64", "X86", "ARM64", runtime.GOARCH}, ctx["arch"])

	// A pinned node architecture is where the job actually runs, so it wins over the
	// runner's: taking the runner's would hand an arm64 pod an x86_64 download url.
	pinned := (&PodEnvironment{input: &PodInput{NodeSelector: map[string]string{"kubernetes.io/arch": "arm64"}}}).
		GetRunnerContext(context.Background())
	assert.Equal(t, "ARM64", pinned["arch"])
}

func TestPodInfoState(t *testing.T) {
	running := podWith(corev1.PodRunning, []corev1.ContainerStatus{{
		Name:  JobContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}, nil)

	info := podInfo(running, JobContainerName)
	assert.Equal(t, "abc-123", info.ID)
	assert.Equal(t, container.StateRunning, info.State)
	assert.Equal(t, container.HealthNone, info.Health, "no readiness probe means no health signal to report")
	assert.NotNil(t, info.Ports, "ports must never be null, it is read in the expression context")

	// A terminated container carries the exit code the job executor checks.
	exited := podWith(corev1.PodFailed, []corev1.ContainerStatus{{
		Name:  JobContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
	}}, nil)

	info = podInfo(exited, JobContainerName)
	assert.Equal(t, "exited", info.State)
	assert.Equal(t, 3, info.ExitCode)

	pending := podWith(corev1.PodPending, nil, nil)
	assert.Equal(t, "created", podInfo(pending, JobContainerName).State)
}

// A readiness probe is the closest equivalent to a docker healthcheck, so it is the
// only case where health is reported as anything but none.
func TestPodInfoHealthFromReadinessProbe(t *testing.T) {
	spec := []corev1.Container{{Name: "redis", ReadinessProbe: &corev1.Probe{}}}

	ready := podWith(corev1.PodRunning, []corev1.ContainerStatus{{
		Name:  "redis",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}, spec)
	assert.Equal(t, container.HealthHealthy, podInfo(ready, "redis").Health)

	starting := podWith(corev1.PodRunning, []corev1.ContainerStatus{{
		Name:  "redis",
		Ready: false,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}}, spec)
	info := podInfo(starting, "redis")
	assert.Equal(t, container.HealthStarting, info.Health)
	assert.Equal(t, "ContainerCreating", info.HealthOutput)
}

func TestPodInfoScopesToNamedContainer(t *testing.T) {
	pod := podWith(corev1.PodRunning, []corev1.ContainerStatus{
		{Name: JobContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{Name: "redis", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
	}, nil)

	assert.Equal(t, container.StateRunning, podInfo(pod, JobContainerName).State)
	assert.Equal(t, 1, podInfo(pod, "redis").ExitCode)
}

func TestIsTerminalWaitingReason(t *testing.T) {
	// These never resolve on their own, so the job fails with the cluster's wording
	// instead of waiting out the scheduling timeout.
	for _, reason := range []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError"} {
		assert.True(t, isTerminalWaitingReason(reason), reason)
	}
	for _, reason := range []string{"", "ContainerCreating", "PodInitializing"} {
		assert.False(t, isTerminalWaitingReason(reason), reason)
	}
}

func TestSchedulingReason(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  "Unschedulable",
		Message: "0/3 nodes are available",
	}}}}
	assert.Equal(t, "Unschedulable: 0/3 nodes are available", schedulingReason(pod))

	scheduled := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
		Type:   corev1.PodScheduled,
		Status: corev1.ConditionTrue,
	}}}}
	assert.Empty(t, schedulingReason(scheduled))
}

func TestReplaceLogWriterRoundTrips(t *testing.T) {
	original := &testWriter{}
	e := &PodEnvironment{input: &PodInput{Stdout: original, Stderr: original}}

	replacement := &testWriter{}
	prevOut, prevErr := e.ReplaceLogWriter(replacement, replacement)

	assert.Same(t, original, prevOut)
	assert.Same(t, original, prevErr)
	assert.Same(t, replacement, e.stdout())
	assert.Same(t, replacement, e.stderr())
}

func TestNewPodEnvironmentFallsBackToClientNamespace(t *testing.T) {
	e := NewPodEnvironment(&Client{Namespace: "from-client"}, &PodInput{Name: "job"})
	assert.Equal(t, "from-client", e.namespace)

	e = NewPodEnvironment(&Client{Namespace: "from-client"}, &PodInput{Name: "job", Namespace: "from-input"})
	assert.Equal(t, "from-input", e.namespace)
}

func TestSidecarViewImplementsExecutionsEnvironment(t *testing.T) {
	pod := NewPodEnvironment(&Client{Namespace: "ci"}, &PodInput{Name: "job"})
	view := NewSidecarView(pod, "redis")

	// A sidecar is created and torn down with its Pod, so its lifecycle methods are
	// no-ops rather than errors.
	require.NoError(t, view.Create(nil, nil)(t.Context()))
	require.NoError(t, view.Start(false)(t.Context()))
	require.NoError(t, view.Pull(false)(t.Context()))
	require.NoError(t, view.Remove()(t.Context()))
	require.NoError(t, view.Close()(t.Context()))
}

type testWriter struct{ data []byte }

func (w *testWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}
