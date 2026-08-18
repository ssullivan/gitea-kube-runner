// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/container/kubernetes"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/require"
)

// errStopBeforeCluster ends startPodEnvironment once it has built its input, so the
// runner-to-backend seam can be asserted on without an API server to talk to.
var errStopBeforeCluster = errors.New("stop before cluster")

type podEnvironmentCall struct {
	cfg   kubernetes.Config
	input *kubernetes.PodInput
}

func capturePodEnvironment(t *testing.T) *podEnvironmentCall {
	t.Helper()

	call := &podEnvironmentCall{}
	original := newPodEnvironment
	t.Cleanup(func() { newPodEnvironment = original })
	newPodEnvironment = func(cfg kubernetes.Config, input *kubernetes.PodInput) (*kubernetes.PodEnvironment, error) {
		call.cfg, call.input = cfg, input
		return nil, errStopBeforeCluster
	}

	return call
}

func TestStartPodEnvironmentBuildsPodInput(t *testing.T) {
	rc := createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest`, ""),
	})
	rc.Config.Workdir = "/workspace"
	rc.Config.Platforms["ubuntu-latest"] = "kubernetes://node:18"
	rc.Config.ContainerMaxLifetime = 90 * time.Second
	rc.Config.ServiceReadyTimeout = 45 * time.Second
	rc.Config.Privileged = true

	runAsUser := int64(1000)
	rc.Config.Kubernetes = KubernetesConfig{
		Namespace:              "ci",
		Kubeconfig:             "/etc/kubeconfig",
		KubeconfigContext:      "prod",
		ServiceAccountName:     "runner-jobs",
		ImagePullSecrets:       []string{"regcred"},
		ImagePullPolicy:        "IfNotPresent",
		PodLabels:              map[string]string{"team": "ci"},
		PodAnnotations:         map[string]string{"owner": "platform"},
		NodeSelector:           map[string]string{"kubernetes.io/arch": "amd64"},
		Tolerations:            []KubernetesToleration{{Key: "ci", Operator: "Equal", Value: "true", Effect: "NoSchedule"}},
		Resources:              KubernetesResources{RequestsCPU: "500m", LimitsMemory: "2Gi"},
		SecurityContext:        KubernetesSecurityContext{RunAsUser: &runAsUser},
		SchedulingTimeout:      7 * time.Minute,
		TerminationGracePeriod: 20 * time.Second,
	}

	call := capturePodEnvironment(t)
	require.ErrorIs(t, rc.startPodEnvironment()(context.Background()), errStopBeforeCluster)

	// The client is resolved from the same settings the pod is created with, so a job
	// cannot end up created against a different cluster than the one configured.
	require.Equal(t, kubernetes.Config{Namespace: "ci", Kubeconfig: "/etc/kubeconfig", KubeconfigContext: "prod"}, call.cfg)

	input := call.input
	require.NotNil(t, input)

	require.Equal(t, "node:18", input.Image, "the label's scheme must be stripped before the image reaches the pod spec")
	require.Equal(t, "/workspace", input.WorkingDir)
	require.Equal(t, []string{"/bin/sleep", "90"}, input.Entrypoint, "the job container has to idle so steps can be exec'd into it")
	require.Equal(t, 90*time.Second, input.MaxLifetime, "the pod's deadline and the idle process must come from one value")
	require.Equal(t, []string{"/workspace", "/var/run/act", "/opt/hostedtoolcache"}, input.MountPaths)
	require.Contains(t, input.Env, "LANG=C.UTF-8")
	require.Contains(t, input.Env, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache")
	require.Equal(t, kubernetes.PodName(input.Name), rc.Env["JOB_CONTAINER_NAME"],
		"steps read the pod's real name from the environment, not the docker-style one it came from")

	require.Equal(t, "ci", input.Namespace)
	require.Equal(t, "runner-jobs", input.ServiceAccountName)
	require.Equal(t, []string{"regcred"}, input.ImagePullSecrets)
	require.Equal(t, "IfNotPresent", input.ImagePullPolicy)
	require.Equal(t, map[string]string{"team": "ci"}, input.Labels)
	require.Equal(t, map[string]string{"owner": "platform"}, input.Annotations)
	require.Equal(t, map[string]string{"kubernetes.io/arch": "amd64"}, input.NodeSelector)
	require.Equal(t, []kubernetes.Toleration{{Key: "ci", Operator: "Equal", Value: "true", Effect: "NoSchedule"}}, input.Tolerations)
	require.Equal(t, kubernetes.PodResources{RequestsCPU: "500m", LimitsMemory: "2Gi"}, input.Resources)
	require.Equal(t, &runAsUser, input.SecurityContext.RunAsUser)
	require.True(t, input.Privileged)
	require.Equal(t, 7*time.Minute, input.SchedulingTimeout)
	require.Equal(t, 20*time.Second, input.TerminationGracePeriod)
	require.Equal(t, 45*time.Second, input.ServiceReadyTimeout)
}

func TestPodServiceInputs(t *testing.T) {
	rc := createIfTestRunContext(map[string]*model.Job{
		"job1": createJob(t, `runs-on: ubuntu-latest
services:
  redis:
    image: "redis:${{ '7' }}"
    env:
      REDIS_PASSWORD: "${{ 'secret' }}"
    cmd: ["--appendonly", "${{ 'yes' }}"]
    volumes:
      - /data:/data
  disabled:
    image: ""
`, ""),
	})
	rc.Config.ProxyEnv = map[string]string{"HTTP_PROXY": "http://proxy:3128"}

	services := rc.podServiceInputs(context.Background())
	require.Len(t, services, 1, "a service with an empty image is skipped rather than started")

	redis := services[0]
	require.Equal(t, "redis", redis.Name)
	require.Equal(t, "redis:7", redis.Image)
	require.Equal(t, []string{"--appendonly", "yes"}, redis.Cmd)
	require.ElementsMatch(t, []string{"REDIS_PASSWORD=secret", "HTTP_PROXY=http://proxy:3128"}, redis.Env)
}

func TestServiceReadyTimeout(t *testing.T) {
	docker := 30 * time.Second
	require.Equal(t, 5*time.Second, serviceReadyTimeout(KubernetesConfig{ServiceReadyTimeout: 5 * time.Second}, docker),
		"the kubernetes section wins when it is set")
	require.Equal(t, docker, serviceReadyTimeout(KubernetesConfig{}, docker),
		"unset falls back to the docker setting rather than becoming no wait at all")
	require.Equal(t, defaultServiceReadyTimeout, serviceReadyTimeout(KubernetesConfig{}, 0),
		"unset everywhere still has to bound the wait, or a stuck sidecar hangs the job")
	require.Equal(t, -1*time.Second, serviceReadyTimeout(KubernetesConfig{ServiceReadyTimeout: -1 * time.Second}, docker),
		"a negative value disables waiting and must not be treated as unset")
}

func TestImagePullPolicy(t *testing.T) {
	require.Equal(t, "IfNotPresent", imagePullPolicy("IfNotPresent", true), "an explicit policy wins over force_pull")
	require.Equal(t, "Always", imagePullPolicy("", true), "there is no pull step to force here, so it becomes a pull policy")
	require.Empty(t, imagePullPolicy("", false), "unset leaves the kubelet its own default")
}

func TestStartPodEnvironmentRefusesCredentials(t *testing.T) {
	// The kubelet pulls the image, so credentials have to reach it as an imagePullSecret.
	// Ignoring them left the Pod in ImagePullBackOff with nothing saying they were unread.
	for name, jobYAML := range map[string]string{
		"job container": `runs-on: ubuntu-latest
container:
  image: private/img
  credentials:
    username: u
    password: p`,
		"service": `runs-on: ubuntu-latest
services:
  db:
    image: private/db
    credentials:
      username: u
      password: p`,
	} {
		t.Run(name, func(t *testing.T) {
			rc := createIfTestRunContext(map[string]*model.Job{"job1": createJob(t, jobYAML, "")})
			rc.Config.Platforms["ubuntu-latest"] = "kubernetes://node:18"
			capturePodEnvironment(t)

			err := rc.startPodEnvironment()(context.Background())
			require.ErrorContains(t, err, "credentials")
			require.ErrorContains(t, err, "unsupported in kubernetes execution mode")
		})
	}
}
