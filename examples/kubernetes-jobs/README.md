## Running jobs as Kubernetes Pods

This example configures a runner to execute each job in its own Kubernetes Pod, using the `kubernetes` label schema. Full description of the backend, its configuration and its limitations: [docs/kubernetes-backend.md](../../docs/kubernetes-backend.md).

> This is **not** the same as [`examples/kubernetes`](../kubernetes), which deploys the runner itself as a Pod with a Docker-in-Docker sidecar and still runs jobs through the Docker backend. Here the runner — wherever it runs — asks the cluster to create a Pod per job. The two can be combined: a runner deployed with those manifests can carry `kubernetes` labels and dispatch jobs as sibling Pods instead of into its sidecar daemon.

Files in this directory:

- [`rbac.yaml`](rbac.yaml)
  The ServiceAccounts, Role, and RoleBinding the runner needs to create, exec into, read logs from, and delete job Pods in its namespace. Apply it before starting the runner.

- [`deployment.yaml`](deployment.yaml)
  The runner itself, running in the cluster: registration token, a small volume for its `.runner` file, its configuration, and the Deployment. No Docker daemon, no privileged sidecar and no mounted socket — a job's isolation comes from being its own Pod. Fill in the image and the token, then apply it after `rbac.yaml`.

  The image has to be built from this fork; upstream's published images do not carry this backend. `make docker` builds the `basic` flavour, which is all this needs, since the bundled-daemon flavours exist to give jobs a daemon this backend never uses.

### Runner configuration

`deployment.yaml` carries this already, in its ConfigMap. It is repeated here because a runner outside the cluster needs the same settings in its own config file: a `kubernetes` label for each image jobs may select, and the `kubernetes:` section pointed at the namespace prepared above.

```yaml
runner:
  labels:
    - "ubuntu-latest:kubernetes://docker.gitea.com/runner-images:ubuntu-latest"

kubernetes:
  namespace: gitea-runner
  # The ServiceAccount job Pods run as; it needs no permissions of its own.
  service_account_name: gitea-runner-jobs
  resources:
    requests_cpu: 500m
    requests_memory: 512Mi
    limits_cpu: "2"
    limits_memory: 4Gi
```

A runner running outside the cluster needs `kubernetes.kubeconfig` set as well; one running inside it picks up its in-cluster credentials automatically, and can leave `namespace` unset to use its own — which is what `deployment.yaml` does.

### Letting the cluster clean up after a runner that dies

A runner deployed *in* the cluster should tell job Pods which Pod it is, by projecting its own name and UID with the downward API. `deployment.yaml` sets both; for any other manifest, add:

```yaml
        env:
          - name: GITEA_RUNNER_POD_NAME
            valueFrom: {fieldRef: {fieldPath: metadata.name}}
          - name: GITEA_RUNNER_POD_UID
            valueFrom: {fieldRef: {fieldPath: metadata.uid}}
```

Job Pods are then owned by the runner's Pod, and Kubernetes deletes them if the runner is killed before it can do so itself — an OOM kill, a node failure, a forced delete. Without it nothing is lost: job Pods still stop at their own deadline, they simply have to be cleaned up afterwards.

This only applies when job Pods share the runner's namespace, which is the default. If `kubernetes.namespace` names a different one the runner leaves the ownership off, because Kubernetes deletes a Pod whose owner is in another namespace — it looks for the owner in the Pod's own namespace and treats it as already gone.

Jobs then select the backend through `runs-on`:

```yaml
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo "this runs in its own Pod"
```
