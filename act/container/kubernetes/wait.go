// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pollInterval is how often the Pod is re-read while waiting for it to start. Polling
// rather than watching keeps the RBAC surface to get/list and survives the watch
// timeouts a long job would otherwise have to re-establish.
const pollInterval = time.Second

// waitForRunning blocks until the job container is running, or returns the reason it
// will never run (an image that cannot be pulled, a Pod that was rejected or failed).
func (e *PodEnvironment) waitForRunning(ctx context.Context, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var lastReason string
	for {
		pod, err := e.client.Clientset.CoreV1().Pods(e.namespace).Get(ctx, e.podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("kubernetes: get pod %s: %w", e.podName, err)
		}

		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded:
			// The phase alone is not enough: a sidecar keeps a Pod Running after the job
			// container has exited, and steps exec'd into a dead container fail with a raw
			// "container not running" instead of the reason it is not.
			switch state := jobContainerState(pod); {
			case state == nil:
				// status not published yet, keep waiting
			case state.Terminated != nil:
				return fmt.Errorf("kubernetes: pod %s: the job container exited before any step ran: %s",
					e.podName, podFailureMessage(pod))
			case state.Running != nil:
				return nil
			}
		case corev1.PodFailed:
			return fmt.Errorf("kubernetes: pod %s failed to start: %s", e.podName, podFailureMessage(pod))
		}

		// A container that will never start (bad image, missing secret) reports it here
		// long before any timeout, so fail on it instead of waiting the timeout out.
		if reason := terminalWaitingReason(pod); reason != "" {
			return fmt.Errorf("kubernetes: pod %s cannot start: %s", e.podName, reason)
		}
		if reason := schedulingReason(pod); reason != "" && reason != lastReason {
			lastReason = reason
		}

		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			detail := lastReason
			if detail == "" {
				detail = string(pod.Status.Phase)
			}
			return fmt.Errorf("kubernetes: timed out waiting for pod %s to start: %s", e.podName, detail)
		}
	}
}

// waitForContainerReady blocks until one container in the Pod reports ready, which is
// the closest equivalent to a docker healthcheck passing for a service container.
func (e *PodEnvironment) waitForContainerReady(ctx context.Context, name string, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for {
		pod, err := e.client.Clientset.CoreV1().Pods(e.namespace).Get(ctx, e.podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("kubernetes: get pod %s: %w", e.podName, err)
		}

		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != name {
				continue
			}
			if status.Ready {
				return nil
			}
			if status.State.Terminated != nil {
				return fmt.Errorf("kubernetes: service container %s exited with code %d", name, status.State.Terminated.ExitCode)
			}
			if reason := waitingReason(status); isTerminalWaitingReason(reason) {
				return fmt.Errorf("kubernetes: service container %s cannot start: %s", name, reason)
			}
		}

		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return fmt.Errorf("kubernetes: timed out waiting for service container %s to be ready", name)
		}
	}
}

func podFailureMessage(pod *corev1.Pod) string {
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "no reason reported"
}

// terminalWaitingReason reports a container waiting state that will not resolve on its
// own, so the job can fail with the cluster's own wording instead of a timeout.
func terminalWaitingReason(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if reason := waitingReason(status); isTerminalWaitingReason(reason) {
			return fmt.Sprintf("%s: %s", status.Name, reason)
		}
	}
	return ""
}

func waitingReason(status corev1.ContainerStatus) string {
	if status.State.Waiting == nil {
		return ""
	}
	if message := status.State.Waiting.Message; message != "" {
		return fmt.Sprintf("%s (%s)", status.State.Waiting.Reason, message)
	}
	return status.State.Waiting.Reason
}

func isTerminalWaitingReason(reason string) bool {
	switch {
	case strings.HasPrefix(reason, "ErrImagePull"),
		strings.HasPrefix(reason, "ImagePullBackOff"),
		strings.HasPrefix(reason, "InvalidImageName"),
		strings.HasPrefix(reason, "CreateContainerConfigError"),
		strings.HasPrefix(reason, "CreateContainerError"):
		return true
	}
	return false
}

// schedulingReason surfaces why an unscheduled Pod is still pending, which is what an
// operator needs when a job Pod waits out its scheduling timeout.
func schedulingReason(pod *corev1.Pod) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status != corev1.ConditionTrue {
			if condition.Message != "" {
				return fmt.Sprintf("%s: %s", condition.Reason, condition.Message)
			}
			return condition.Reason
		}
	}
	return ""
}

// jobContainerState is the job container's own state within the Pod, or nil before the
// kubelet has published one.
func jobContainerState(pod *corev1.Pod) *corev1.ContainerState {
	for i, status := range pod.Status.ContainerStatuses {
		if status.Name == JobContainerName {
			return &pod.Status.ContainerStatuses[i].State
		}
	}
	return nil
}
