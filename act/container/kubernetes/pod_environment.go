// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ container.ExecutionsEnvironment = (*PodEnvironment)(nil)

// PodEnvironment runs a job in a single Kubernetes Pod: one container for the job's
// steps, one sidecar per service container, and an emptyDir shared by both. It
// implements container.ExecutionsEnvironment so the job executor and step runners are
// unchanged from the docker backend.
type PodEnvironment struct {
	container.LinuxContainerEnvironmentExtensions

	client    *Client
	input     *PodInput
	namespace string
	podName   string
}

// RunnerContext is what a job Pod knows about itself, for the `runner` expression context
// and the RUNNER_* variables. It answers arch from the runner's own architecture: the shared
// Linux extensions ask a docker daemon instead, which in a cluster has none to ask, so every
// step of every job both warned into the job log and left runner.arch empty.
func RunnerContext() map[string]any {
	return map[string]any{
		"os":         "Linux",
		"arch":       runnerArch(),
		"temp":       "/tmp",
		"tool_cache": container.DefaultToolCache,
	}
}

// runnerArch maps to the values GitHub documents for runner.arch, as the docker backend does.
func runnerArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "386":
		return "X86"
	case "arm64":
		return "ARM64"
	}
	return runtime.GOARCH
}

func (e *PodEnvironment) GetRunnerContext(context.Context) map[string]any {
	return RunnerContext()
}

// NewPodEnvironment returns the execution environment for a job Pod. The Pod itself is
// only created by Create.
func NewPodEnvironment(client *Client, input *PodInput) *PodEnvironment {
	namespace := input.Namespace
	if namespace == "" {
		namespace = client.Namespace
	}
	// Resolve it back onto the input so the spec is built against the namespace the Pod is
	// actually created in. The owner reference is only emitted when it matches, and left
	// empty it never would.
	input.Namespace = namespace

	return &PodEnvironment{
		client:    client,
		input:     input,
		namespace: namespace,
		podName:   PodName(input.Name),
	}
}

// Create creates the job Pod. The job container runs a long-lived no-op command so the
// steps can be exec'd into it afterwards, the same shape as the docker backend's
// create-then-exec model.
func (e *PodEnvironment) Create(capAdd, capDrop []string) common.Executor {
	return func(ctx context.Context) error {
		pod, err := BuildPod(e.input, capAdd, capDrop)
		if err != nil {
			return err
		}

		created, err := e.client.Clientset.CoreV1().Pods(e.namespace).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("kubernetes: create pod %s: %w", e.podName, err)
		}
		e.podName = created.Name

		common.Logger(ctx).Debugf("Created pod %s/%s", e.namespace, e.podName)

		return nil
	}
}

// ConnectToNetwork is a no-op: every container in a Pod already shares one network
// namespace, which is what the docker backend uses a per-job network for.
func (e *PodEnvironment) ConnectToNetwork(_ string) common.Executor {
	return func(_ context.Context) error { return nil }
}

// Start waits for the job container to be running, and for each service sidecar to
// report ready, which stands in for the docker healthcheck this backend has no
// equivalent of.
func (e *PodEnvironment) Start(_ bool) common.Executor {
	return func(ctx context.Context) error {
		if err := e.waitForRunning(ctx, e.input.SchedulingTimeout); err != nil {
			return err
		}

		if timeout := e.input.ServiceReadyTimeout; timeout >= 0 {
			for _, service := range e.input.Services {
				if err := e.waitForContainerReady(ctx, service.Name, timeout); err != nil {
					return err
				}
			}
		}

		return nil
	}
}

// Pull is a no-op: the kubelet pulls images when it starts the Pod. force_pull is
// honoured as an ImagePullPolicy on the Pod spec instead, so it applies at create time
// rather than as a step of its own.
func (e *PodEnvironment) Pull(_ bool) common.Executor {
	return func(_ context.Context) error { return nil }
}

func (e *PodEnvironment) Copy(destPath string, files ...*container.FileEntry) common.Executor {
	return func(ctx context.Context) error {
		return e.copyContent(ctx, destPath, files...)
	}
}

func (e *PodEnvironment) CopyTarStream(ctx context.Context, destPath string, tarStream io.Reader) error {
	return e.copyTarStream(ctx, destPath, tarStream)
}

func (e *PodEnvironment) CopyDir(destPath, srcPath string, useGitIgnore bool) common.Executor {
	return func(ctx context.Context) error {
		return e.copyDir(ctx, destPath, srcPath, useGitIgnore)
	}
}

func (e *PodEnvironment) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	return e.getContainerArchive(ctx, srcPath)
}

// Inspect maps the Pod's status onto the docker-shaped Info the job executor reads.
func (e *PodEnvironment) Inspect(ctx context.Context) (*container.Info, error) {
	pod, err := e.client.Clientset.CoreV1().Pods(e.namespace).Get(ctx, e.podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kubernetes: pod %s %w", e.podName, container.ErrContainerNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("kubernetes: inspect pod %s: %w", e.podName, err)
	}

	return podInfo(pod, JobContainerName), nil
}

// podInfo maps one container's status within a Pod onto container.Info. Kubernetes has
// no docker-style HEALTHCHECK, so readiness is the closest signal there is: a ready
// container reports healthy, one still starting reports starting.
func podInfo(pod *corev1.Pod, containerName string) *container.Info {
	info := &container.Info{
		ID:     string(pod.UID),
		State:  podState(pod.Status.Phase),
		Health: container.HealthNone,
		Ports:  map[string]string{}, // never null, it is read in the expression context
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != containerName {
			continue
		}
		switch {
		case status.State.Terminated != nil:
			info.State = "exited"
			info.ExitCode = int(status.State.Terminated.ExitCode)
		case status.State.Running != nil:
			info.State = container.StateRunning
		case status.State.Waiting != nil:
			info.State = "created"
		}
		if hasReadinessProbe(pod, containerName) {
			if status.Ready {
				info.Health = container.HealthHealthy
			} else {
				info.Health = container.HealthStarting
			}
			if reason := waitingReason(status); reason != "" {
				info.HealthOutput = reason
			}
		}
		break
	}

	return info
}

func hasReadinessProbe(pod *corev1.Pod, containerName string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			return c.ReadinessProbe != nil
		}
	}
	return false
}

func podState(phase corev1.PodPhase) string {
	switch phase {
	case corev1.PodRunning:
		return container.StateRunning
	case corev1.PodSucceeded, corev1.PodFailed:
		return "exited"
	default:
		return "created"
	}
}

func (e *PodEnvironment) DumpLogs(ctx context.Context) error {
	return e.dumpLogs(ctx, JobContainerName)
}

func (e *PodEnvironment) Exec(command []string, env map[string]string, user, workdir string) common.Executor {
	return common.NewPipelineExecutor(
		common.NewInfoExecutor("kubernetes exec cmd=[%s] user=%s workdir=%s", strings.Join(command, " "), user, workdir),
		func(ctx context.Context) error {
			if user != "" {
				// The exec subresource runs as the container's own user; there is no
				// per-exec equivalent of docker's --user.
				common.Logger(ctx).Debugf("kubernetes: ignoring user %q for exec, the pod's securityContext decides", user)
			}
			return e.exec(ctx, execOptions{
				container: JobContainerName,
				command:   command,
				env:       env,
				workdir:   workdir,
				stdout:    e.stdout(),
				stderr:    e.stderr(),
				tty:       e.input.AllocatePTY,
			})
		},
	).IfNot(common.Dryrun)
}

func (e *PodEnvironment) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return container.ParseEnvFile(e, srcPath, env).IfNot(common.Dryrun)
}

// UpdateFromImageEnv reads the effective environment out of the running job container.
// The kubelet exposes no image config to read it from before the Pod runs, unlike the
// docker backend's image inspect.
func (e *PodEnvironment) UpdateFromImageEnv(env *map[string]string) common.Executor {
	return func(ctx context.Context) error {
		var out strings.Builder
		if err := e.exec(ctx, execOptions{
			container: JobContainerName,
			command:   []string{"env"},
			stdout:    &out,
		}); err != nil {
			// Not fatal: the job's own env still applies, this only seeds it with the
			// image's own variables.
			common.Logger(ctx).Debugf("kubernetes: could not read the image environment: %v", err)
			return nil
		}

		localEnv := *env
		for line := range strings.SplitSeq(out.String(), "\n") {
			if name, value, found := strings.Cut(strings.TrimRight(line, "\r"), "="); found && name != "" {
				if _, exists := localEnv[name]; !exists {
					localEnv[name] = value
				}
			}
		}
		*env = localEnv

		return nil
	}
}

// Remove deletes the job Pod, taking its service sidecars and the workspace emptyDir
// with it.
func (e *PodEnvironment) Remove() common.Executor {
	return func(ctx context.Context) error {
		if e.podName == "" {
			return nil
		}

		grace := int64(0)
		if g := e.input.TerminationGracePeriod; g > 0 {
			grace = int64(g.Seconds())
		}

		err := e.client.Clientset.CoreV1().Pods(e.namespace).Delete(ctx, e.podName, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("kubernetes: delete pod %s: %w", e.podName, err)
		}

		common.Logger(ctx).Debugf("Removed pod %s/%s", e.namespace, e.podName)

		return nil
	}
}

// Close is a no-op: the client holds no per-job resource to release, and the Pod is
// removed by Remove.
func (e *PodEnvironment) Close() common.Executor {
	return func(_ context.Context) error { return nil }
}

func (e *PodEnvironment) ReplaceLogWriter(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	previousStdout := e.input.Stdout
	previousStderr := e.input.Stderr

	e.input.Stdout = stdout
	e.input.Stderr = stderr

	return previousStdout, previousStderr
}
