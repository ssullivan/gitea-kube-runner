// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: prod
clusters:
  - name: prod-cluster
    cluster:
      server: https://prod.example:6443
  - name: staging-cluster
    cluster:
      server: https://staging.example:6443
contexts:
  - name: prod
    context:
      cluster: prod-cluster
      user: prod-user
      namespace: prod-jobs
  - name: staging
    context:
      cluster: staging-cluster
      user: staging-user
      namespace: staging-jobs
users:
  - name: prod-user
    user:
      token: prod-token
  - name: staging-user
    user:
      token: staging-token
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(testKubeconfig), 0o600))
	return path
}

func TestResolveNamespace(t *testing.T) {
	assert.Equal(t, "configured", resolveNamespace("configured", "discovered"))
	assert.Equal(t, "discovered", resolveNamespace("", "discovered"))
	assert.Equal(t, "default", resolveNamespace("", ""))
}

func TestNewClientUsesKubeconfigCurrentContext(t *testing.T) {
	cli, err := NewClient(Config{Kubeconfig: writeKubeconfig(t)})
	require.NoError(t, err)

	assert.Equal(t, "https://prod.example:6443", cli.RESTMap.Host)
	assert.Equal(t, "prod-jobs", cli.Namespace)
}

func TestNewClientHonoursKubeconfigContext(t *testing.T) {
	cli, err := NewClient(Config{Kubeconfig: writeKubeconfig(t), KubeconfigContext: "staging"})
	require.NoError(t, err)

	assert.Equal(t, "https://staging.example:6443", cli.RESTMap.Host)
	assert.Equal(t, "staging-jobs", cli.Namespace)
}

// An explicitly configured namespace wins over the one the kubeconfig context names.
func TestNewClientConfiguredNamespaceWins(t *testing.T) {
	cli, err := NewClient(Config{Kubeconfig: writeKubeconfig(t), Namespace: "ci"})
	require.NoError(t, err)

	assert.Equal(t, "ci", cli.Namespace)
}

func TestNewClientMissingKubeconfig(t *testing.T) {
	_, err := NewClient(Config{Kubeconfig: filepath.Join(t.TempDir(), "absent")})
	require.Error(t, err)
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	restConfig := &rest.Config{Host: server.URL}
	clientset, err := corev1.NewForConfig(restConfig)
	require.NoError(t, err)

	return &Client{Clientset: clientset, RESTMap: restConfig, Namespace: "ci"}
}

func TestPingReachableCluster(t *testing.T) {
	cli := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/version", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"33","gitVersion":"v1.33.0"}`))
	})

	require.NoError(t, cli.Ping(context.Background()))
}

func TestPingUnreachableCluster(t *testing.T) {
	cli := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	err := cli.Ping(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot reach the kubernetes cluster")
}
