#!/usr/bin/env bash
# Kubernetes execution backend test harness.
#
# Runs the cluster-facing tests of the kubernetes backend against a real cluster. Those
# tests exercise the pod exec and log subresources and tar-over-exec file copying, none
# of which a fake clientset or envtest can stand in for: envtest runs only etcd and the
# apiserver, with no kubelet to actually run a container.
#
# By default it creates a throwaway kind cluster and deletes it afterwards. Point
# KUBECONFIG at an existing cluster to use that instead, which is faster to iterate on.
#
# Usage: scripts/test-k8s.sh [-- go-test-args...]
#
# Env:
#   KUBECONFIG              use this cluster instead of creating one with kind
#   K8S_TEST_CLUSTER        kind cluster name (default gitea-runner-test)
#   K8S_TEST_NAMESPACE      namespace job pods are created in (default gitea-runner-test)
#   K8S_TEST_PRELOAD        space-separated images to side-load into kind (default alpine:latest)
set -euo pipefail

[ "${1:-}" = "--" ] && shift
[ $# -eq 0 ] && set -- -race -count=1 -run '^TestKubernetes' ./act/container/kubernetes/

cluster="${K8S_TEST_CLUSTER:-gitea-runner-test}"
namespace="${K8S_TEST_NAMESPACE:-gitea-runner-test}"
preload="${K8S_TEST_PRELOAD:-alpine:latest}"
created_cluster=""
kubeconfig_path=""

if [ -n "${KUBECONFIG:-}" ]; then
  echo "==> Using the cluster in KUBECONFIG=$KUBECONFIG"
else
  # Same self-skip philosophy as `make test`: no tooling, no failure.
  for tool in kind kubectl docker; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "==> $tool not found, skipping the kubernetes backend tests"
      exit 0
    fi
  done
  if ! docker info >/dev/null 2>&1; then
    echo "==> docker is not available, skipping the kubernetes backend tests"
    exit 0
  fi
fi

cleanup() {
  if [ -n "$created_cluster" ]; then
    echo "==> Deleting kind cluster $cluster"
    kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
  fi
  # Written whenever this script makes its own, cluster created here or reused, so it is
  # removed on both paths rather than left in /tmp by every local run.
  if [ -n "$kubeconfig_path" ]; then
    rm -f "$kubeconfig_path"
  fi
}
trap cleanup EXIT

if [ -z "${KUBECONFIG:-}" ]; then
  if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
    echo "==> Reusing existing kind cluster $cluster"
  else
    echo "==> Creating kind cluster $cluster"
    kind create cluster --name "$cluster" --wait 120s
    created_cluster="yes"
  fi

  kubeconfig_path="$(mktemp)"
  kind get kubeconfig --name "$cluster" > "$kubeconfig_path"
  export KUBECONFIG="$kubeconfig_path"

  # Side-load the test images so the tests need no registry access, mirroring how
  # test-dind.sh preloads into its fresh daemon.
  for image in $preload; do
    if ! docker image inspect "$image" >/dev/null 2>&1; then
      echo "==> Pulling $image"
      docker pull "$image"
    fi
    echo "==> Loading $image into $cluster"
    kind load docker-image "$image" --name "$cluster"
  done
fi

echo "==> Ensuring namespace $namespace"
kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f -

export GITEA_RUNNER_TEST_NAMESPACE="$namespace"

echo "==> Running: go test $*"
go test "$@"
