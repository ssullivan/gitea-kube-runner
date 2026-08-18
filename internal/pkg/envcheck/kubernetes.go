// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package envcheck

import (
	"context"

	"gitea.com/gitea/runner/act/container/kubernetes"
)

// CheckIfKubernetesReachable verifies the cluster the kubernetes execution backend
// would create job Pods on is reachable with the configured credentials.
func CheckIfKubernetesReachable(ctx context.Context, cfg kubernetes.Config) error {
	cli, err := kubernetes.NewClient(cfg)
	if err != nil {
		return err
	}

	return cli.Ping(ctx)
}
