// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"fmt"
	"io"
	"os"

	"gitea.com/gitea/runner/act/common"

	corev1 "k8s.io/api/core/v1"
)

// dumpLogs writes a container's log so far to the environment's output writers. The
// stream is already demultiplexed by the kubelet, unlike the docker backend's, so it
// is copied straight through and stays subject to the same line-writer contract.
func (e *PodEnvironment) dumpLogs(ctx context.Context, containerName string) error {
	request := e.client.Clientset.CoreV1().Pods(e.namespace).GetLogs(e.podName, &corev1.PodLogOptions{
		Container: containerName,
	})

	stream, err := request.Stream(ctx)
	if err != nil {
		return fmt.Errorf("kubernetes: read logs of %s in pod %s: %w", containerName, e.podName, err)
	}
	defer stream.Close()

	out := e.stdout()
	_, err = io.Copy(out, stream)
	common.FlushWriter(out)

	return err
}

func (e *PodEnvironment) stdout() io.Writer {
	if e.input.Stdout != nil {
		return e.input.Stdout
	}
	return os.Stdout
}

func (e *PodEnvironment) stderr() io.Writer {
	if e.input.Stderr != nil {
		return e.input.Stderr
	}
	return os.Stderr
}
