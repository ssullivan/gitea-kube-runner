// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/container"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These exercise the parts that cannot be tested without a real kubelet: exec, log
// streaming, tar-over-exec copying and sidecar reachability. A fake clientset has no
// server behind the exec and log subresources, and envtest has no container runtime at
// all, so a real cluster (scripts/test-k8s.sh spins one up with kind) is required.
// Without KUBECONFIG pointing at one they self-skip, like the docker-facing tests do.

// testImage is deliberately small and has the busybox tar/sh the backend needs.
const testImage = "alpine:latest"

func integrationClient(t *testing.T) *Client {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("KUBECONFIG is not set, skipping the cluster-facing tests (run make test-k8s)")
	}

	namespace := os.Getenv("GITEA_RUNNER_TEST_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	client, err := NewClient(Config{Kubeconfig: kubeconfig, Namespace: namespace})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Skipf("cluster in KUBECONFIG is not reachable, skipping: %v", err)
	}

	return client
}

// startPod creates a job Pod that idles so steps can be exec'd into it, and removes it
// when the test ends.
func startPod(t *testing.T, input *PodInput) (*PodEnvironment, context.Context) {
	t.Helper()

	client := integrationClient(t)
	input.Name = fmt.Sprintf("gitea-runner-test-%d-%s", time.Now().UnixNano(), strings.ToLower(t.Name()[:min(8, len(t.Name()))]))
	input.Name = strings.NewReplacer("/", "-", "_", "-").Replace(input.Name)
	if input.Image == "" {
		input.Image = testImage
	}
	if input.Entrypoint == nil {
		input.Entrypoint = []string{"/bin/sleep", "600"}
	}
	if input.SchedulingTimeout == 0 {
		input.SchedulingTimeout = 3 * time.Minute
	}
	if input.ImagePullPolicy == "" {
		// :latest defaults to Always, which re-pulls and defeats the side-load these tests
		// rely on to need no registry.
		input.ImagePullPolicy = "IfNotPresent"
	}

	env := NewPodEnvironment(client, input)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	require.NoError(t, env.Create(nil, nil)(ctx))
	t.Cleanup(func() {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), time.Minute)
		defer removeCancel()
		if err := env.Remove()(removeCtx); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	require.NoError(t, env.Start(false)(ctx))

	return env, ctx
}

func TestKubernetesPodLifecycle(t *testing.T) {
	var out bytes.Buffer
	env, ctx := startPod(t, &PodInput{Stdout: &out, Stderr: &out, MountPaths: []string{"/workspace"}})

	info, err := env.Inspect(ctx)
	require.NoError(t, err)
	assert.Equal(t, container.StateRunning, info.State)

	require.NoError(t, env.Exec([]string{"sh", "-c", "echo hello from the pod"}, nil, "", "")(ctx))
	assert.Contains(t, out.String(), "hello from the pod")

	// The Pod is gone after Remove, which is what frees the workspace emptyDir too.
	require.NoError(t, env.Remove()(ctx))
	_, err = env.client.Clientset.CoreV1().Pods(env.namespace).Get(ctx, env.podName, metav1.GetOptions{})
	require.Error(t, err)
}

func TestKubernetesExecReportsExitCode(t *testing.T) {
	var out bytes.Buffer
	env, ctx := startPod(t, &PodInput{Stdout: &out, Stderr: &out})

	err := env.Exec([]string{"sh", "-c", "exit 7"}, nil, "", "")(ctx)
	require.Error(t, err)

	var exitErr container.ExitCodeError
	require.ErrorAs(t, err, &exitErr, "a failing step must surface as ExitCodeError, as it does on docker")
	assert.Equal(t, 7, int(exitErr))
}

func TestKubernetesExecAppliesEnvAndWorkdir(t *testing.T) {
	var out bytes.Buffer
	env, ctx := startPod(t, &PodInput{Stdout: &out, Stderr: &out, MountPaths: []string{"/workspace"}})

	require.NoError(t, env.Exec([]string{"sh", "-c", "echo $GREETING from $(pwd)"},
		map[string]string{"GREETING": "hello world"}, "", "/workspace")(ctx))

	assert.Contains(t, out.String(), "hello world from /workspace")
}

func TestKubernetesCopyRoundTrip(t *testing.T) {
	var out bytes.Buffer
	env, ctx := startPod(t, &PodInput{Stdout: &out, Stderr: &out, MountPaths: []string{"/workspace"}})

	require.NoError(t, env.Copy("/workspace", &container.FileEntry{
		Name: "greeting.txt",
		Mode: 0o644,
		Body: "hello from the runner\n",
	})(ctx))

	require.NoError(t, env.Exec([]string{"cat", "/workspace/greeting.txt"}, nil, "", "")(ctx))
	assert.Contains(t, out.String(), "hello from the runner")

	// And back out again, as a tar stream, which is how the env files are read.
	archive, err := env.GetContainerArchive(ctx, "/workspace/greeting.txt")
	require.NoError(t, err)
	defer archive.Close()

	content, err := io.ReadAll(archive)
	require.NoError(t, err)
	assert.Contains(t, string(content), "hello from the runner")
}

// UpdateFromEnv is how a step's exported variables reach the following steps, so it has
// to work against a real tar stream out of the Pod.
func TestKubernetesUpdateFromEnv(t *testing.T) {
	var out bytes.Buffer
	env, ctx := startPod(t, &PodInput{Stdout: &out, Stderr: &out, MountPaths: []string{"/workspace"}})

	require.NoError(t, env.Copy("/workspace", &container.FileEntry{
		Name: "envs.txt",
		Mode: 0o644,
		Body: "FOO=bar\nBAZ=qux\n",
	})(ctx))

	got := map[string]string{}
	require.NoError(t, env.UpdateFromEnv("/workspace/envs.txt", &got)(ctx))
	assert.Equal(t, map[string]string{"FOO": "bar", "BAZ": "qux"}, got)
}

func TestKubernetesDumpLogs(t *testing.T) {
	var out bytes.Buffer
	env, ctx := startPod(t, &PodInput{
		Stdout:     &out,
		Stderr:     &out,
		Entrypoint: []string{"sh", "-c", "echo job container speaking; sleep 600"},
	})

	require.NoError(t, env.DumpLogs(ctx))
	assert.Contains(t, out.String(), "job container speaking")
}

// Service containers become sidecars in the job's Pod, so they are reachable on
// localhost rather than by name. This is the assumption the whole one-Pod-per-job
// design rests on, so it is worth an end-to-end check.
func TestKubernetesServiceSidecarReachableOnLocalhost(t *testing.T) {
	var out bytes.Buffer
	env, ctx := startPod(t, &PodInput{
		Stdout: &out,
		Stderr: &out,
		Services: []ServiceInput{{
			Name:       "web",
			Image:      testImage,
			Entrypoint: []string{"sh", "-c", "while true; do printf 'HTTP/1.1 200 OK\\r\\n\\r\\nhi from the sidecar\\n' | nc -l -p 8080; done"},
		}},
		ServiceReadyTimeout: -1, // no readiness probe on this image, so do not wait for one
	})

	require.Eventually(t, func() bool {
		out.Reset()
		return env.Exec([]string{"sh", "-c", "wget -qO- http://localhost:8080"}, nil, "", "")(ctx) == nil
	}, time.Minute, 2*time.Second, "the sidecar should become reachable on localhost")

	assert.Contains(t, out.String(), "hi from the sidecar")

	// The sidecar's own status and logs are readable through its view.
	view := NewSidecarView(env, "web")
	info, err := view.Inspect(ctx)
	require.NoError(t, err)
	assert.Equal(t, container.StateRunning, info.State)
}

// TestKubernetesOwnedPodIsCollectedWithItsOwner proves the mechanism the runner relies on to
// clean up after itself when it dies without deleting its job Pods: the cluster's garbage
// collector removes them once the owner is gone. Only a real API server has a garbage
// collector, so this cannot be asserted anywhere but here.
func TestKubernetesOwnedPodIsCollectedWithItsOwner(t *testing.T) {
	client := integrationClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	pods := client.Clientset.CoreV1().Pods(client.Namespace)
	ownerName := fmt.Sprintf("gitea-runner-test-owner-%d", time.Now().UnixNano())
	owner, err := pods.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: ownerName},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "owner", Image: testImage, Command: []string{"/bin/sleep", "600"}}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pods.Delete(context.Background(), ownerName, metav1.DeleteOptions{})
	})

	env, _ := startPod(t, &PodInput{
		Namespace: client.Namespace,
		Owner:     &PodOwner{Name: owner.Name, UID: string(owner.UID), Namespace: client.Namespace},
	})

	created, err := pods.Get(ctx, env.podName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, created.OwnerReferences, 1, "the job pod should be owned by the runner's pod")
	require.Equal(t, owner.UID, created.OwnerReferences[0].UID)

	require.NoError(t, pods.Delete(ctx, ownerName, metav1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		_, err := pods.Get(ctx, env.podName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 2*time.Second, "deleting the owner should have the cluster collect the job pod")
}
