// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package kubernetes runs jobs as Kubernetes Pods, one Pod per job, as an
// alternative to the docker and host execution backends. See
// docs/kubernetes-backend.md.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// inClusterNamespaceFile is where the kubelet projects a Pod's own namespace.
const inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Config is the subset of the runner's kubernetes configuration this package needs
// to build a client. It mirrors config.Kubernetes without importing it, keeping the
// vendored act tree independent of the runner's config package.
type Config struct {
	Namespace         string
	Kubeconfig        string
	KubeconfigContext string
}

// Client is a Kubernetes API client plus the REST config it was built from. The
// REST config is kept because the exec and attach subresources need it to build
// their own SPDY round trippers, which the clientset does not expose.
type Client struct {
	Clientset corev1.Interface
	RESTMap   *rest.Config
	Namespace string
}

// NewClient resolves credentials the way kubectl does: in-cluster when the runner
// itself runs in a Pod and no kubeconfig is configured, otherwise the kubeconfig
// (explicit path, then the usual KUBECONFIG/$HOME/.kube/config rules).
func NewClient(cfg Config) (*Client, error) {
	restConfig, namespace, err := resolveRESTConfig(cfg)
	if err != nil {
		return nil, err
	}

	clientset, err := corev1.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	return &Client{Clientset: clientset, RESTMap: restConfig, Namespace: namespace}, nil
}

func resolveRESTConfig(cfg Config) (*rest.Config, string, error) {
	// An explicit kubeconfig or context always wins, so a runner inside a cluster can
	// still be pointed at a different one.
	if cfg.Kubeconfig == "" && cfg.KubeconfigContext == "" {
		if restConfig, err := rest.InClusterConfig(); err == nil {
			return restConfig, resolveNamespace(cfg.Namespace, inClusterNamespace()), nil
		} else if !isNotInCluster(err) {
			return nil, "", fmt.Errorf("load in-cluster kubernetes config: %w", err)
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.KubeconfigContext != "" {
		overrides.CurrentContext = cfg.KubeconfigContext
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubernetes config: %w", err)
	}

	// A kubeconfig context carries its own namespace, which is the closest thing to
	// "where this operator works" when the config does not name one.
	contextNamespace, _, err := clientConfig.Namespace()
	if err != nil {
		contextNamespace = ""
	}

	return restConfig, resolveNamespace(cfg.Namespace, contextNamespace), nil
}

// isNotInCluster reports whether InClusterConfig failed only because the process is
// not running in a Pod, as opposed to a real configuration error worth surfacing.
func isNotInCluster(err error) bool {
	return errors.Is(err, rest.ErrNotInCluster)
}

// OwnerFromEnv is the runner's own Pod, as projected by the downward API, or nil when the
// runner is not running as one. Its namespace comes from the ServiceAccount token rather than
// the environment, because that file *is* the Pod's own namespace, where the configured
// kubernetes.namespace may well be a different one.
//
// Nil is the safe answer and the previous behaviour: job Pods are simply not owned, and a
// runner that dies without deleting them leaves them to their activeDeadlineSeconds. So a
// deployment that does not project these stays exactly as it was.
func OwnerFromEnv() *PodOwner {
	name, uid := os.Getenv("GITEA_RUNNER_POD_NAME"), os.Getenv("GITEA_RUNNER_POD_UID")
	namespace := inClusterNamespace()
	if name == "" || uid == "" || namespace == "" {
		return nil
	}
	return &PodOwner{Name: name, UID: uid, Namespace: namespace}
}

func inClusterNamespace() string {
	data, err := os.ReadFile(inClusterNamespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func resolveNamespace(configured, discovered string) string {
	if configured != "" {
		return configured
	}
	if discovered != "" {
		return discovered
	}
	return "default"
}

// Ping verifies the cluster is reachable and the credentials are accepted.
func (c *Client) Ping(ctx context.Context) error {
	// /version is the cheapest authenticated call that needs no RBAC of its own, so it
	// checks reachability without masking a missing Pod permission. Issued through the REST
	// client rather than Discovery, which takes no context: connect_timeout has to bound
	// this, and an unreachable address otherwise blocks for the OS connect timeout.
	if err := c.Clientset.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error(); err != nil {
		return fmt.Errorf("cannot reach the kubernetes cluster: %w", err)
	}
	return nil
}
