// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"fmt"
	"io"
	"maps"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// JobContainerName is the name of the container a job's steps are executed in. Service
// containers are named after their service id, so this must not collide with one.
const JobContainerName = "job"

// workspaceVolumeName is the emptyDir shared by every container in the Pod. It backs
// both the workspace and the runner's own act path, which have no persistence across
// jobs in this backend.
const workspaceVolumeName = "gitea-runner-workspace"

// PodResources are CPU/memory requests and limits in Kubernetes quantity syntax.
// An empty string leaves that field unset.
type PodResources struct {
	RequestsCPU    string
	RequestsMemory string
	LimitsCPU      string
	LimitsMemory   string
}

// PodSecurityContext are Pod-level securityContext defaults. Pointers distinguish
// unset from false/zero.
type PodSecurityContext struct {
	RunAsNonRoot *bool
	RunAsUser    *int64
	FSGroup      *int64
}

// PodOwner is the runner's own Pod. Naming it as a job Pod's owner lets the cluster's
// garbage collector remove job Pods the runner did not live to delete itself.
type PodOwner struct {
	Name      string
	UID       string
	Namespace string
}

// ownerReferences returns what to put on a job Pod in namespace, which is nothing unless the
// owner is fully known and lives in that same namespace.
//
// The namespace check is not a formality. Kubernetes does not ignore a cross-namespace owner
// reference: the garbage collector looks for the owner in the *dependent's* namespace, does not
// find it, and deletes the dependent as orphaned — killing a running job within seconds of it
// starting. Anything unproven here has to mean no owner reference at all.
func ownerReferences(owner *PodOwner, namespace string) []metav1.OwnerReference {
	if owner == nil || owner.Name == "" || owner.UID == "" || owner.Namespace == "" {
		return nil
	}
	if owner.Namespace != namespace {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       owner.Name,
		UID:        types.UID(owner.UID),
	}}
}

// Toleration mirrors the subset of corev1.Toleration the runner exposes.
type Toleration struct {
	Key      string
	Operator string
	Value    string
	Effect   string
}

// ServiceInput is one of a job's service containers, which becomes a sidecar in the
// job's Pod rather than a container of its own.
type ServiceInput struct {
	Name       string
	Image      string
	Entrypoint []string
	Cmd        []string
	Env        []string
}

// PodInput describes the Pod to run a job in. It is the kubernetes analogue of
// container.NewContainerInput, and deliberately carries no bind mounts, docker network
// or published ports: the workspace is always the shared emptyDir, and services are
// reachable over the Pod's shared network namespace on localhost.
type PodInput struct {
	Name        string
	Image       string
	Entrypoint  []string
	Cmd         []string
	WorkingDir  string
	Env         []string
	Stdout      io.Writer
	Stderr      io.Writer
	AllocatePTY bool
	Services    []ServiceInput

	// Mounted into every container, so the workspace, act path and tool cache the
	// job's steps use all resolve to the same emptyDir.
	MountPaths []string

	// MaxLifetime bounds the Pod the way the job container's sleep entrypoint already
	// bounds its process, but enforced by the kubelet, so a Pod the runner never got to
	// delete still stops on its own.
	MaxLifetime time.Duration

	// Owner is the runner's own Pod, when it has one and job Pods share its namespace.
	Owner *PodOwner

	Namespace              string
	ServiceAccountName     string
	ImagePullSecrets       []string
	ImagePullPolicy        string
	Labels                 map[string]string
	Annotations            map[string]string
	NodeSelector           map[string]string
	Tolerations            []Toleration
	Resources              PodResources
	SecurityContext        PodSecurityContext
	Privileged             bool
	TerminationGracePeriod time.Duration
	SchedulingTimeout      time.Duration
	ServiceReadyTimeout    time.Duration
}

// invalidPodNameChars matches everything an RFC 1123 subdomain may not contain.
var invalidPodNameChars = regexp.MustCompile(`[^a-z0-9.-]`)

// PodName adapts a job's container name to Kubernetes, which requires a lowercase RFC 1123
// subdomain where docker accepted the mixed-case names the runner builds for both backends.
// Length needs no handling: the runner caps the readable part and appends a sha256, well
// inside the 253 character limit.
func PodName(name string) string {
	name = invalidPodNameChars.ReplaceAllString(strings.ToLower(name), "-")
	name = strings.Trim(name, ".-")
	if name == "" {
		return "gitea-runner-job"
	}
	return name
}

// BuildPod turns a PodInput into the Pod object to create. It is pure so the spec can
// be asserted on without a cluster.
func BuildPod(input *PodInput, capAdd, capDrop []string) (*corev1.Pod, error) {
	resources, err := buildResources(input.Resources)
	if err != nil {
		return nil, err
	}

	mounts := buildVolumeMounts(input.MountPaths)

	containers := []corev1.Container{{
		Name:            JobContainerName,
		Image:           input.Image,
		Command:         input.Entrypoint,
		Args:            input.Cmd,
		WorkingDir:      input.WorkingDir,
		Env:             buildEnv(input.Env),
		ImagePullPolicy: corev1.PullPolicy(input.ImagePullPolicy),
		VolumeMounts:    mounts,
		Resources:       resources,
		SecurityContext: buildContainerSecurityContext(input.Privileged, capAdd, capDrop),
		// Steps are executed with the exec subresource against this container, which
		// needs a TTY allocated up front for a step that asks for one.
		TTY:   input.AllocatePTY,
		Stdin: input.AllocatePTY,
	}}

	for _, service := range input.Services {
		containers = append(containers, corev1.Container{
			Name:            service.Name,
			Image:           service.Image,
			Command:         service.Entrypoint,
			Args:            service.Cmd,
			Env:             buildEnv(service.Env),
			ImagePullPolicy: corev1.PullPolicy(input.ImagePullPolicy),
			VolumeMounts:    mounts,
			SecurityContext: buildContainerSecurityContext(input.Privileged, nil, nil),
		})
	}

	labels := map[string]string{"app.kubernetes.io/managed-by": "gitea-runner"}
	maps.Copy(labels, input.Labels)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            PodName(input.Name),
			Namespace:       input.Namespace,
			Labels:          labels,
			Annotations:     input.Annotations,
			OwnerReferences: ownerReferences(input.Owner, input.Namespace),
		},
		Spec: corev1.PodSpec{
			// A job Pod is a one-shot workload: never restart it behind the runner's
			// back, so a crashed step surfaces as a failure instead of a retry.
			RestartPolicy:      corev1.RestartPolicyNever,
			Containers:         containers,
			Volumes:            []corev1.Volume{{Name: workspaceVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			ServiceAccountName: input.ServiceAccountName,
			NodeSelector:       input.NodeSelector,
			Tolerations:        buildTolerations(input.Tolerations),
			SecurityContext:    buildPodSecurityContext(input.SecurityContext),
		},
	}

	for _, secret := range input.ImagePullSecrets {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: secret})
	}

	if grace := input.TerminationGracePeriod; grace > 0 {
		seconds := int64(grace.Round(time.Second).Seconds())
		pod.Spec.TerminationGracePeriodSeconds = &seconds
	}

	if life := input.MaxLifetime; life > 0 {
		seconds := int64(life.Round(time.Second).Seconds())
		pod.Spec.ActiveDeadlineSeconds = &seconds
	}

	return pod, nil
}

// buildVolumeMounts mounts the one workspace volume at each requested path, using a
// subPath per mount so they stay distinct directories inside the same volume.
func buildVolumeMounts(paths []string) []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		mounts = append(mounts, corev1.VolumeMount{
			Name:      workspaceVolumeName,
			MountPath: path,
			SubPath:   subPathFor(path),
		})
	}
	return mounts
}

// subPathFor derives a stable, filesystem-safe directory name for a mount path.
func subPathFor(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "root"
	}
	return strings.ReplaceAll(trimmed, "/", "_")
}

func buildEnv(env []string) []corev1.EnvVar {
	vars := make([]corev1.EnvVar, 0, len(env))
	for _, entry := range env {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			continue
		}
		vars = append(vars, corev1.EnvVar{Name: name, Value: value})
	}
	return vars
}

func buildResources(res PodResources) (corev1.ResourceRequirements, error) {
	requirements := corev1.ResourceRequirements{}

	for _, field := range []struct {
		value string
		name  corev1.ResourceName
		into  *corev1.ResourceList
		label string
	}{
		{res.RequestsCPU, corev1.ResourceCPU, &requirements.Requests, "requests_cpu"},
		{res.RequestsMemory, corev1.ResourceMemory, &requirements.Requests, "requests_memory"},
		{res.LimitsCPU, corev1.ResourceCPU, &requirements.Limits, "limits_cpu"},
		{res.LimitsMemory, corev1.ResourceMemory, &requirements.Limits, "limits_memory"},
	} {
		if field.value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(field.value)
		if err != nil {
			return requirements, fmt.Errorf("kubernetes.resources.%s %q is not a valid quantity: %w", field.label, field.value, err)
		}
		if *field.into == nil {
			*field.into = corev1.ResourceList{}
		}
		(*field.into)[field.name] = quantity
	}

	return requirements, nil
}

func buildPodSecurityContext(sc PodSecurityContext) *corev1.PodSecurityContext {
	if sc.RunAsNonRoot == nil && sc.RunAsUser == nil && sc.FSGroup == nil {
		return nil
	}
	return &corev1.PodSecurityContext{
		RunAsNonRoot: sc.RunAsNonRoot,
		RunAsUser:    sc.RunAsUser,
		FSGroup:      sc.FSGroup,
	}
}

func buildContainerSecurityContext(privileged bool, capAdd, capDrop []string) *corev1.SecurityContext {
	if !privileged && len(capAdd) == 0 && len(capDrop) == 0 {
		return nil
	}

	sc := &corev1.SecurityContext{}
	if privileged {
		// Passed through rather than rejected: whether a privileged job Pod is allowed
		// is the cluster's call, enforced by Pod Security Admission.
		sc.Privileged = &privileged
	}
	if len(capAdd) > 0 || len(capDrop) > 0 {
		sc.Capabilities = &corev1.Capabilities{}
		for _, add := range capAdd {
			sc.Capabilities.Add = append(sc.Capabilities.Add, corev1.Capability(add))
		}
		for _, drop := range capDrop {
			sc.Capabilities.Drop = append(sc.Capabilities.Drop, corev1.Capability(drop))
		}
	}
	return sc
}

func buildTolerations(tolerations []Toleration) []corev1.Toleration {
	if len(tolerations) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, 0, len(tolerations))
	for _, t := range tolerations {
		out = append(out, corev1.Toleration{
			Key:      t.Key,
			Operator: corev1.TolerationOperator(t.Operator),
			Value:    t.Value,
			Effect:   corev1.TaintEffect(t.Effect),
		})
	}
	return out
}
