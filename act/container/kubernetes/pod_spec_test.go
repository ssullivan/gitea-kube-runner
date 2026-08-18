// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

func minimalInput() *PodInput {
	return &PodInput{
		Name:       "job-pod",
		Image:      "node:18",
		Namespace:  "ci",
		WorkingDir: "/workspace",
		MountPaths: []string{"/workspace", "/var/run/act"},
	}
}

func containerByName(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("container %q not found in pod", name)
	return corev1.Container{}
}

func TestBuildPodJobContainer(t *testing.T) {
	input := minimalInput()
	input.Entrypoint = []string{"/bin/sleep", "3600"}
	input.Env = []string{"FOO=bar", "EMPTY=", "malformed"}

	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, "job-pod", pod.Name)
	assert.Equal(t, "ci", pod.Namespace)
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)

	job := containerByName(t, pod, JobContainerName)
	assert.Equal(t, "node:18", job.Image)
	assert.Equal(t, []string{"/bin/sleep", "3600"}, job.Command)
	assert.Equal(t, "/workspace", job.WorkingDir)
	// A malformed entry without "=" is dropped rather than becoming an empty name.
	assert.Equal(t, []corev1.EnvVar{{Name: "FOO", Value: "bar"}, {Name: "EMPTY", Value: ""}}, job.Env)
}

func TestBuildPodNameIsValidForKubernetes(t *testing.T) {
	// The name the runner builds for both backends: docker takes it as-is, kubernetes
	// rejects the upper case.
	input := minimalInput()
	input.Name = "GITEA-ACTIONS-TASK-1-WORKFLOW-k8s-smoke-JOB-smoke-40f7d32f61d4"

	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "gitea-actions-task-1-workflow-k8s-smoke-job-smoke-40f7d32f61d4", pod.Name)

	assert.Equal(t, "job.pod-1", PodName("Job.Pod_1"), "an underscore is not allowed either")
	assert.Equal(t, "abc", PodName("-.abc.-"), "a name must start and end alphanumeric")
	assert.Equal(t, "gitea-runner-job", PodName("---"), "a name that sanitizes to nothing still has to be valid")
}

func TestBuildPodBoundsItsOwnLifetime(t *testing.T) {
	// Without this the only bound is the sleep entrypoint, inside the container. A Pod the
	// runner died before deleting would keep that process alive to the end of its sleep with
	// nothing able to cut it short.
	input := minimalInput()
	input.MaxLifetime = 90 * time.Minute

	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(5400), *pod.Spec.ActiveDeadlineSeconds)

	unset, err := BuildPod(minimalInput(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, unset.Spec.ActiveDeadlineSeconds, "unset must leave the pod without a deadline, not one of zero")
}

func TestBuildPodOwnerReference(t *testing.T) {
	owner := &PodOwner{Name: "gitea-runner-abc", UID: "11111111-2222-3333-4444-555555555555", Namespace: "ci"}

	input := minimalInput() // namespace "ci"
	input.Owner = owner
	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)
	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "Pod", pod.OwnerReferences[0].Kind)
	assert.Equal(t, "gitea-runner-abc", pod.OwnerReferences[0].Name)
	assert.Equal(t, types.UID("11111111-2222-3333-4444-555555555555"), pod.OwnerReferences[0].UID)

	// The one that matters. Kubernetes does not ignore a cross-namespace owner: its garbage
	// collector looks for the owner in the job Pod's namespace, does not find it, and deletes
	// the job Pod. Emitting one here would kill running jobs seconds after they start.
	elsewhere := minimalInput()
	elsewhere.Namespace = "other"
	elsewhere.Owner = owner
	pod, err = BuildPod(elsewhere, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, pod.OwnerReferences, "an owner in another namespace must not be referenced at all")

	for name, o := range map[string]*PodOwner{
		"no owner":     nil,
		"no name":      {UID: "u", Namespace: "ci"},
		"no uid":       {Name: "n", Namespace: "ci"},
		"no namespace": {Name: "n", UID: "u"},
	} {
		partial := minimalInput()
		partial.Owner = o
		pod, err := BuildPod(partial, nil, nil)
		require.NoError(t, err)
		assert.Empty(t, pod.OwnerReferences, "%s: anything unproven has to mean no owner reference", name)
	}
}

func TestAssignServiceContainerNames(t *testing.T) {
	// A service id is the workflow author's and may hold anything YAML allows. An underscore
	// alone had the API server reject the whole Pod, so the job failed before a step ran.
	services := []ServiceInput{
		{Name: "my_db"},
		{Name: "Redis Cache"},
		{Name: "job"},   // would collide with the job container
		{Name: "my-db"}, // collides with my_db once sanitised
		{Name: "_"},     // nothing survives sanitising
		{Name: strings.Repeat("x", 80)},
	}
	AssignServiceContainerNames(services)

	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	seen := map[string]bool{JobContainerName: false}
	for _, s := range services {
		assert.LessOrEqual(t, len(s.ContainerName), maxContainerName, "%q is over the label limit", s.Name)
		assert.Regexp(t, valid, s.ContainerName, "%q produced a name kubernetes rejects", s.Name)
		assert.NotEqual(t, JobContainerName, s.ContainerName, "%q collided with the job container", s.Name)
		assert.False(t, seen[s.ContainerName], "%q reused the name %q", s.Name, s.ContainerName)
		seen[s.ContainerName] = true
	}

	assert.Equal(t, "svc-my-db", services[0].ContainerName)
	assert.Equal(t, "svc-redis-cache", services[1].ContainerName)
	assert.Equal(t, "svc-job", services[2].ContainerName)
	assert.Equal(t, "svc-my-db-2", services[3].ContainerName, "a collision has to be broken, not silently shared")
	assert.Equal(t, "svc-service", services[4].ContainerName)

	// The id stays the user's handle: job.services.<id> and the logs still use it.
	assert.Equal(t, "my_db", services[0].Name)
}

func TestBuildPodMountsOneVolumeAtEachPath(t *testing.T) {
	input := minimalInput()
	input.MountPaths = []string{"/workspace", "/var/run/act", "/workspace", ""}

	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)

	require.Len(t, pod.Spec.Volumes, 1)
	assert.Equal(t, workspaceVolumeName, pod.Spec.Volumes[0].Name)
	require.NotNil(t, pod.Spec.Volumes[0].EmptyDir, "the workspace is ephemeral, scoped to the pod")

	job := containerByName(t, pod, JobContainerName)
	// Duplicates and empties are dropped; each path gets its own subPath so they stay
	// distinct directories within the one volume.
	require.Len(t, job.VolumeMounts, 2)
	assert.Equal(t, "/workspace", job.VolumeMounts[0].MountPath)
	assert.Equal(t, "workspace", job.VolumeMounts[0].SubPath)
	assert.Equal(t, "/var/run/act", job.VolumeMounts[1].MountPath)
	assert.Equal(t, "var_run_act", job.VolumeMounts[1].SubPath)
}

func TestBuildPodServicesBecomeSidecars(t *testing.T) {
	input := minimalInput()
	input.Services = []ServiceInput{
		{Name: "redis", Image: "redis:7", Env: []string{"A=1"}},
		{Name: "postgres", Image: "postgres:16", Cmd: []string{"-c", "log_statement=all"}},
	}
	AssignServiceContainerNames(input.Services)

	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)

	require.Len(t, pod.Spec.Containers, 3)
	redis := containerByName(t, pod, "svc-redis")
	assert.Equal(t, "redis:7", redis.Image)
	assert.Equal(t, []corev1.EnvVar{{Name: "A", Value: "1"}}, redis.Env)

	postgres := containerByName(t, pod, "svc-postgres")
	assert.Equal(t, []string{"-c", "log_statement=all"}, postgres.Args)
	// Sidecars share the workspace so a service can read fixtures the job staged.
	assert.Equal(t, containerByName(t, pod, JobContainerName).VolumeMounts, postgres.VolumeMounts)
}

func TestBuildPodSchedulingAndPullSettings(t *testing.T) {
	input := minimalInput()
	input.ImagePullPolicy = "Always"
	input.ImagePullSecrets = []string{"regcred", "other"}
	input.ServiceAccountName = "runner-jobs"
	input.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	input.Tolerations = []Toleration{{Key: "ci", Operator: "Equal", Value: "true", Effect: "NoSchedule"}}
	input.Labels = map[string]string{"team": "platform"}
	input.Annotations = map[string]string{"note": "hello"}
	input.TerminationGracePeriod = 45 * time.Second

	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, corev1.PullAlways, containerByName(t, pod, JobContainerName).ImagePullPolicy)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "regcred"}, {Name: "other"}}, pod.Spec.ImagePullSecrets)
	assert.Equal(t, "runner-jobs", pod.Spec.ServiceAccountName)
	assert.Equal(t, map[string]string{"kubernetes.io/arch": "amd64"}, pod.Spec.NodeSelector)
	require.Len(t, pod.Spec.Tolerations, 1)
	assert.Equal(t, corev1.TaintEffectNoSchedule, pod.Spec.Tolerations[0].Effect)
	assert.Equal(t, "platform", pod.Labels["team"])
	assert.Equal(t, "gitea-runner", pod.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, map[string]string{"note": "hello"}, pod.Annotations)
	require.NotNil(t, pod.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(45), *pod.Spec.TerminationGracePeriodSeconds)
}

func TestBuildPodResources(t *testing.T) {
	input := minimalInput()
	input.Resources = PodResources{RequestsCPU: "500m", RequestsMemory: "512Mi", LimitsMemory: "2Gi"}

	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)

	res := containerByName(t, pod, JobContainerName).Resources
	assert.Equal(t, resource.MustParse("500m"), res.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("512Mi"), res.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("2Gi"), res.Limits[corev1.ResourceMemory])
	_, hasCPULimit := res.Limits[corev1.ResourceCPU]
	assert.False(t, hasCPULimit, "an unset limit stays unset rather than defaulting to zero")
}

func TestBuildPodRejectsInvalidQuantity(t *testing.T) {
	input := minimalInput()
	input.Resources = PodResources{RequestsCPU: "half a core"}

	_, err := BuildPod(input, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes.resources.requests_cpu")
}

func TestBuildPodSecurityContext(t *testing.T) {
	input := minimalInput()
	pod, err := BuildPod(input, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, pod.Spec.SecurityContext, "an unset security context leaves the cluster defaults alone")
	assert.Nil(t, containerByName(t, pod, JobContainerName).SecurityContext)

	nonRoot := true
	uid := int64(1000)
	input.SecurityContext = PodSecurityContext{RunAsNonRoot: &nonRoot, RunAsUser: &uid}
	input.Privileged = true

	pod, err = BuildPod(input, []string{"NET_ADMIN"}, []string{"MKNOD"})
	require.NoError(t, err)

	require.NotNil(t, pod.Spec.SecurityContext)
	assert.Equal(t, &nonRoot, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.Equal(t, &uid, pod.Spec.SecurityContext.RunAsUser)

	sc := containerByName(t, pod, JobContainerName).SecurityContext
	require.NotNil(t, sc)
	require.NotNil(t, sc.Privileged)
	assert.True(t, *sc.Privileged)
	assert.Equal(t, []corev1.Capability{"NET_ADMIN"}, sc.Capabilities.Add)
	assert.Equal(t, []corev1.Capability{"MKNOD"}, sc.Capabilities.Drop)
}
