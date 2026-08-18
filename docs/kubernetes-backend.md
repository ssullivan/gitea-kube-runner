# Kubernetes execution backend

The Kubernetes backend runs each job in its own Pod on a cluster, instead of in a Docker container on the runner's host. It is selected per job by a runner label using the `kubernetes` schema:

```text
ubuntu-latest:kubernetes://docker.gitea.com/runner-images:ubuntu-latest
```

A job whose `runs-on` matches that label gets a Pod created from the label's image; the job's steps are executed in it over the Kubernetes exec API, and the Pod is deleted when the job finishes.

> **Not to be confused with [`examples/kubernetes/`](../examples/kubernetes/)**, which deploys *the runner itself* as a Pod with a Docker-in-Docker sidecar and still executes jobs through the Docker backend. The two are independent and composable: a runner deployed that way can use `kubernetes` labels to dispatch jobs as sibling Pods rather than into its own sidecar daemon.

## Configuration

The backend is configured under `kubernetes:` in the runner YAML config (see [config.example.yaml](../internal/pkg/config/config.example.yaml)):

```yaml
runner:
  labels:
    - "ubuntu-latest:kubernetes://docker.gitea.com/runner-images:ubuntu-latest"

kubernetes:
  namespace: ci
  service_account_name: gitea-runner-jobs
  resources:
    requests_cpu: 500m
    requests_memory: 512Mi
    limits_cpu: "2"
    limits_memory: 4Gi
```

Credentials are resolved the same way `kubectl` resolves them: in-cluster configuration when the runner itself runs in a Pod, otherwise `kubeconfig` (or the usual `KUBECONFIG`/`$HOME/.kube/config` rules). `namespace` defaults to the runner's own namespace in-cluster, or `default` outside one.

Set `require_kubernetes: true` to make the runner refuse to start unless the cluster is reachable, even when no label requires it; `connect_timeout` bounds that startup check.

## Cluster permissions

The runner needs to create, inspect, exec into, read logs from, and delete Pods in its namespace. A namespaced Role is enough — see [`examples/kubernetes-jobs/rbac.yaml`](../examples/kubernetes-jobs/rbac.yaml):

| Resource | Verbs |
| --- | --- |
| `pods` | `create`, `get`, `list`, `watch`, `delete` |
| `pods/exec` | `create` |
| `pods/log` | `get` |

## Workspace and services

The job's workspace is an `emptyDir` volume shared by every container in the Pod. It lives and dies with the Pod, so **nothing is cached between jobs** — including the tool cache that the Docker backend keeps in a named volume. Actions that download tooling re-download it on each job.

A job's `services:` become sidecar containers in the same Pod. Because containers in a Pod share a network namespace, a service is reached on `localhost` at its own port rather than by service name. `service_ready_timeout` bounds how long the job waits for them before starting the steps; a service whose image declares no readiness probe reports no health, so set it to a negative value to start the steps without waiting.

Service `volumes:`, `ports:` and `options:` are ignored: the sidecar already shares the Pod's network, so its ports need no publishing, and there is no per-service volume model here.

The job image must provide `sh` and `tar`, which the backend uses to stage files into the workspace over the exec API — the same implicit requirement `docker cp` places on images today.

## Cache and artifacts

Both work. Artifacts travel through Gitea and need nothing special. The cache is served by the runner itself, so a job Pod reaches it at whatever address the runner advertises — which is the ordinary expectation and holds in both deployments: a runner in the cluster is reached on its Pod IP, and a runner outside it on its host address.

It only needs attention where the cluster has no route back to the runner: one behind NAT or on an isolated network, or a job Pod whose egress a NetworkPolicy denies. There the cache steps fail while the rest of the job succeeds. Set `cache.host` to an address the cluster can reach, or point `cache.external_server` at a shared cache server.

`actions/cache` shells out to `tar` with GNU options, so a job image whose `tar` is BusyBox (Alpine's default) fails to save the cache even though the backend copied everything correctly. Install GNU `tar` in the image, or use one that ships it. This is not specific to this backend.

## If the runner dies mid-job

A graceful stop is unaffected: cancelling and shutting down both delete the job Pod. The question is what happens when the runner never gets to, after an OOM kill, a node failure or a forced delete.

Two things limit the damage, and the second has to be switched on.

**Every job Pod carries an `activeDeadlineSeconds`** equal to the job's maximum lifetime — the runner's own timeout, or the time remaining until the task's deadline. The cluster terminates an abandoned Pod even with nothing left to ask it to; the Pod object then remains, as `DeadlineExceeded`, until something removes it. This always applies.

**A runner running in the cluster can own its job Pods**, so Kubernetes deletes them as soon as the runner's Pod is gone, rather than at the deadline. It needs to know which Pod it is, which the downward API provides:

```yaml
        env:
          - name: GITEA_RUNNER_POD_NAME
            valueFrom: {fieldRef: {fieldPath: metadata.name}}
          - name: GITEA_RUNNER_POD_UID
            valueFrom: {fieldRef: {fieldPath: metadata.uid}}
```

Absent these, job Pods are simply not owned and the deadline is the only bound — which is also what happens for a runner outside the cluster, and when `kubernetes.namespace` puts job Pods in a namespace other than the runner's own. That last exclusion is deliberate rather than an omission: Kubernetes looks for an owner in the *dependent's* namespace, so a cross-namespace reference makes it treat the owner as already deleted and remove the job Pod within seconds of it starting. The runner leaves the ownership off rather than risk that.

Neither mechanism covers the runner's container restarting inside a Pod that survives — the Pod object is still there, so there is nothing for the collector to act on, and the deadline applies as usual.

To clean up after such a crash:

```bash
kubectl delete pod -n <namespace> -l app.kubernetes.io/managed-by=gitea-runner
```

## Limitations

Job Pods have no access to a Docker daemon, so anything that needs one is unsupported and fails the job with a clear error:

- `uses: docker://<image>` steps
- actions with `runs.using: docker` (including local `Dockerfile` actions, which would need to be built)

Steps that shell out to `docker` themselves (`docker build`, `docker compose`, ...) cannot be detected in advance and will simply fail inside the job with `docker: command not found` or a connection error. Use the Docker backend for workflows that need Docker.

`container.privileged` maps to the Pod's security context; whether it is allowed is the cluster's decision, and a Pod Security Admission rejection surfaces as a job start failure naming the policy.

Steps run as the image's own user: the exec API has no per-command equivalent of docker's `--user`, so `kubernetes.pod_security_context` decides instead.

## Testing

`make test-k8s` runs the backend's cluster-facing tests against a throwaway [kind](https://kind.sigs.k8s.io/) cluster, or against `KUBECONFIG` when one is already set. They cover what a fake client cannot: exec, log streaming, tar-over-exec copying, and reaching a service sidecar on localhost. It self-skips when kind or docker is unavailable.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| Job fails after `scheduling_timeout` | No node satisfies the Pod's resources, `node_selector`, or tolerations. Check `kubectl get events` in the namespace. |
| `ImagePullBackOff` | The image is private and no matching `image_pull_secrets` entry was configured, or the name is wrong. |
| Pod creation rejected | Pod Security Admission or an admission webhook refused the spec — often `run_as_non_root` or a privileged setting. |
| `forbidden` errors on startup or job start | The runner's ServiceAccount is missing one of the verbs in the table above. |
