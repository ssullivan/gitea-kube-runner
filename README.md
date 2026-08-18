# Gitea Runner

> **A fork of [gitea/runner](https://gitea.com/gitea/runner) that adds a Kubernetes execution backend.**
>
> It is a drop-in replacement for the upstream runner. The CLI, the configuration file, the
> registration flow and the existing `docker` and `host` backends are unchanged, and the new
> configuration is additive, so an existing runner can be swapped for this binary without
> editing its config or re-registering.
>
> What it adds is a third execution backend that runs each job as its own Kubernetes Pod,
> opted into per job by a `kubernetes://` label — no Docker daemon, no privileged
> Docker-in-Docker sidecar. See [docs/kubernetes-backend.md](docs/kubernetes-backend.md) for
> its configuration, cluster permissions and limitations.
>
> The pre-built binaries linked below are upstream's and do **not** include this backend;
> build from source to get it.

## Installation

### Prerequisites

Docker Engine Community version is required for docker mode. To install Docker CE, follow the official [install instructions](https://docs.docker.com/engine/install/).

### Download pre-built binary

Visit [here](https://dl.gitea.com/gitea-runner/) and download the right version for your platform.

### Build from source

```bash
make build
```

### Build a docker image

```bash
make docker
```

## Quickstart

Actions are disabled by default, so you need to add the following to the configuration file of your Gitea instance to enable it:

```ini
[actions]
ENABLED=true
```

### Register

```bash
./gitea-runner register
```

And you will be asked to input:

1. Gitea instance URL, like `http://192.168.8.8:3000/`. You should use your gitea instance ROOT_URL as the instance argument
 and you should not use `localhost` or `127.0.0.1` as instance IP;
2. Runner token, you can get it from `http://192.168.8.8:3000/admin/actions/runners`;
3. Runner name, you can just leave it blank;
4. Runner labels, you can just leave it blank.

The process looks like:

```text
INFO Registering runner, arch=amd64, os=darwin, version=0.1.5.
WARN Runner in user-mode.
INFO Enter the Gitea instance URL (for example, https://gitea.com/):
http://192.168.8.8:3000/
INFO Enter the runner token:
fe884e8027dc292970d4e0303fe82b14xxxxxxxx
INFO Enter the runner name (if set empty, use hostname: Test.local):

INFO Enter the runner labels, leave blank to use the default labels (comma-separated, for example, ubuntu-latest:docker://docker.gitea.com/runner-images:ubuntu-latest):

INFO Registering runner, name=Test.local, instance=http://192.168.8.8:3000/, labels=[ubuntu-latest:docker://docker.gitea.com/runner-images:ubuntu-latest ubuntu-22.04:docker://docker.gitea.com/runner-images:ubuntu-22.04 ubuntu-20.04:docker://docker.gitea.com/runner-images:ubuntu-20.04].
DEBU Successfully pinged the Gitea instance server
INFO Runner registered successfully.
```

You can also register with command line arguments.

```bash
./gitea-runner register --instance http://192.168.8.8:3000 --token <my_runner_token> --no-interactive
```

If the registry succeed, it will run immediately. Next time, you could run the runner directly.

### Run

```bash
./gitea-runner daemon
```

### Run with docker

```bash
docker run -e GITEA_INSTANCE_URL=https://your_gitea.com -e GITEA_RUNNER_REGISTRATION_TOKEN=<your_token> -v /var/run/docker.sock:/var/run/docker.sock --name my_runner gitea/runner:nightly
```

Mount a volume on `/data` if you want the registration file and optional config to survive container recreation (see [scripts/run.sh](scripts/run.sh)).

> **`/data` does not hold the image cache.** It is the runner's working directory and contains only the `.runner` registration file and, optionally, your config file. Images pulled for jobs live in the *Docker daemon's* data root, which for the `dind` flavours is inside the container (`/var/lib/docker`, or `/home/rootless/.local/share/docker` for `dind-rootless`). To keep the image cache across restarts, give that path its own volume as well — otherwise every new container re-pulls the job images. With the `basic` flavour the images live on whichever daemon you point the runner at, so there is nothing extra to persist.

### Image flavours

The image is published in three flavours, all built from the single multi-stage [Dockerfile](Dockerfile) in this repository. They differ only in how a Docker daemon is made available to the jobs the runner executes; the `gitea-runner` binary inside them is identical.

| Tag | Build target | Base image | Docker daemon | Process supervisor | Runs as |
| --- | --- | --- | --- | --- | --- |
| `latest` (and `<version>`) | `basic` | `alpine` | none — uses an external daemon you provide | [`tini`](https://github.com/krallin/tini) | `root` |
| `latest-dind` | `dind` | `docker:dind` | bundled, started inside the container | [`s6`](https://skarnet.org/software/s6/) | `root` (privileged) |
| `latest-dind-rootless` | `dind-rootless` | `docker:dind-rootless` | bundled, started rootless inside the container | [`s6`](https://skarnet.org/software/s6/) | `rootless` (UID 1000) |

#### `latest` — basic

The default flavour ships only the runner on a minimal Alpine base. It contains **no Docker daemon of its own**: jobs that use `docker://` images need a daemon supplied from outside the container, typically by bind-mounting the host's socket:

```bash
docker run -e GITEA_INSTANCE_URL=https://your_gitea.com -e GITEA_RUNNER_REGISTRATION_TOKEN=<your_token> \
  -v /var/run/docker.sock:/var/run/docker.sock --name my_runner gitea/runner:latest
```

`tini` is the entrypoint (it reaps zombie processes), and it just runs [`scripts/run.sh`](scripts/run.sh), which registers the runner on first start and then execs `gitea-runner daemon`. This flavour does not need `--privileged`. The trade-off is that jobs share the host's daemon, so they can see other containers and images on that daemon.

#### `latest-dind` — Docker-in-Docker

This flavour is based on the official `docker:dind` image and bundles its own Docker daemon, so it needs no external socket — only the `--privileged` flag that Docker-in-Docker requires:

```bash
docker run --privileged -e GITEA_INSTANCE_URL=https://your_gitea.com -e GITEA_RUNNER_REGISTRATION_TOKEN=<your_token> \
  --name my_runner gitea/runner:latest-dind
```

Two processes have to run side by side here (the Docker daemon and the runner), so the entrypoint is the [`s6`](https://skarnet.org/software/s6/) supervision tree under [`scripts/s6`](scripts/s6) instead of `tini`. `s6` starts `dockerd`, and the runner service waits for the daemon to come up (`s6-svwait`) before launching [`run.sh`](scripts/run.sh). Each container has a private daemon isolated from the host's, at the cost of running privileged.

#### `latest-dind-rootless` — rootless Docker-in-Docker

Same idea as `dind`, but built on `docker:dind-rootless` so the bundled daemon and the runner run as an unprivileged user (`rootless`, UID 1000) rather than `root`. `DOCKER_HOST` is preset to `unix:///run/user/1000/docker.sock` so the runner talks to the rootless daemon. This reduces the blast radius compared to the privileged `dind` flavour, but rootless Docker carries the usual rootless limitations (networking, cgroups, storage drivers, and some operations that need additional host configuration such as `/etc/subuid` / `/etc/subgid` mappings and unprivileged user-namespace support).

> **The UID is fixed at 1000.** It comes from the `rootless` user baked into the upstream `docker:dind-rootless` base image, and the bundled daemon always listens on `/run/user/1000/docker.sock` inside the container, so running this flavour as a different user (`--user 1001`) does not work. If you need the runner to talk to a *host* rootless daemon that runs under some other UID, use the `basic` flavour instead and bind-mount that daemon's socket (see [examples/vm/rootless-docker.md](examples/vm/rootless-docker.md)); pointing `DOCKER_HOST` at a host socket from inside `dind-rootless` will not work. Changing the UID otherwise means rebuilding the image from a base with a different `rootless` user.

> **Note on Podman:** these images target the Docker daemon. The bundled `dind`/`dind-rootless` daemons are `dockerd`, not Podman, and the `basic` flavour expects a Docker-compatible socket. Running them under rootless Podman is not a supported configuration, though pointing the `basic` flavour at a Podman socket that emulates the Docker API may work for some workloads.

### Configuration

The runner reads a YAML file. Without one, every option keeps its default.

```bash
./gitea-runner config init             # write config.yaml, with no option set
./gitea-runner config generate | less  # read what the options do
./gitea-runner -c config.yaml daemon   # -c also works on register and cache-server
```

`config generate` prints [config.example.yaml](internal/pkg/config/config.example.yaml). Every value in it is commented out, so copy the lines you want to change into your own file and uncomment them.

#### Editing a config file

`config` edits a file in place, which is handy in provisioning scripts:

```bash
./gitea-runner config set runner.capacity 4
./gitea-runner config set runner.timeout 90m  # written as 1h30m0s
./gitea-runner config set runner.envs.MY_VAR value
./gitea-runner config add runner.labels 'ubuntu:docker://node:22'
./gitea-runner config remove runner.labels 'ubuntu:docker://node:22'
./gitea-runner config get runner.labels
```

A key is its dotted YAML path. An unknown key, a value of the wrong type, or `add`/`remove` on anything but a list is refused before the file is touched. `set` replaces a whole list when you give it several values.

An edit keeps the comments and the key order of the file. Indentation becomes two spaces, and a blank line between two values is dropped.

`config get`, `set`, `add` and `remove` use `config.yaml` (or `config.yml`) from the working directory, then from the directory of the binary, and print their choice to stderr. `config init` writes `config.yaml` in the working directory, and refuses to overwrite an existing config without `--force`. Pass `-c` for another path.

#### Environment variables

Earlier releases let a few environment variables (`GITEA_DEBUG`, `GITEA_TRACE`, `GITEA_RUNNER_CAPACITY`, `GITEA_RUNNER_FILE`, `GITEA_RUNNER_ENVIRON`, `GITEA_RUNNER_ENV_FILE`) override parts of the config. They are gone, use the YAML file for all settings. The Docker images still read their own variables, such as `RUNNER_STATE_FILE`, see [scripts/run.sh](scripts/run.sh) and the container documentation below.

### Labels

Labels decide **which jobs a runner accepts** and **how it runs them**. A job's `runs-on` is matched against the runner's label names; the first match wins and selects the execution environment for that job.

A label is written as:

```text
<name>[:<schema>[:<args>]]
```

| Part | Meaning |
| --- | --- |
| `name` | The name a workflow refers to in `runs-on`, e.g. `ubuntu-latest`. |
| `schema` | One of `docker`, `host`, or `kubernetes`. Defaults to `host` when omitted. |
| `args` | Used by the `docker` and `kubernetes` schemas: the image to run the job in. |

Three schemas are supported:

- **`docker://<image>`** — the job runs inside a container created from `<image>`:

  ```text
  ubuntu-latest:docker://docker.gitea.com/runner-images:ubuntu-latest
  ```

- **`host`** — the job's steps run directly on the machine the runner is on, using the tools installed there:

  ```text
  macos:host
  ```

- **`kubernetes://<image>`** — the job runs in its own Kubernetes Pod created from `<image>`:

  ```text
  ubuntu-latest:kubernetes://docker.gitea.com/runner-images:ubuntu-latest
  ```

  See [docs/kubernetes-backend.md](docs/kubernetes-backend.md) for the required configuration, cluster permissions, and limitations.

So with the labels

```text
ubuntu-latest:docker://docker.gitea.com/runner-images:ubuntu-latest,macos:host
```

a workflow with `runs-on: ubuntu-latest` is executed in the `runner-images:ubuntu-latest` container, and one with `runs-on: macos` is executed directly on the host.

Names may themselves contain a colon (for example `pool:e57e18d4-10d4-406f-93bf-60f127221bdd`); only `host`, `docker`, and `kubernetes` are treated as schemas.

If a job's `runs-on` matches none of the runner's labels, the job still runs, in the default `docker.gitea.com/runner-images:ubuntu-latest` image. Images maintained for this purpose are listed at [gitea/runner-images](https://gitea.com/gitea/runner-images).

Labels are chosen at registration time (`--labels`, or the interactive prompt) and can be changed afterwards by editing `runner.labels` in the config file, or in the Gitea UI under the runner's settings.

#### Registration vs config labels

If `runner.labels` is set in the YAML file, those labels are used during `register` and the `--labels` CLI flag is ignored.

The `daemon` command also accepts `--labels` (which defaults to the `GITEA_RUNNER_LABELS` environment variable), so the labels of an already registered runner can be changed without deleting its registration file. The most explicit source wins:

```
--labels / GITEA_RUNNER_LABELS   >   runner.labels in the config file   >   labels in the .runner file
```

Whenever the resulting labels differ from the ones in the registration file, they are written back to it and re-declared to the Gitea instance on startup.

> **Note:** A runner that only exposes `host` labels still needs access to a Docker daemon (e.g. a mounted `/var/run/docker.sock`) whenever a job uses a `docker://` action or a service container. `host` labels only change where the job's own steps run; container-based steps and actions are still executed with Docker.
>
> A job that sets `container:` is the same story, and the two schemas differ here. On a `host` label, an explicit `container:` image runs the **whole job in Docker** rather than on the host. On a `kubernetes://` label it does not: there is no daemon to fall back to, so `container:` only changes the image the job's Pod is created from. Every job's log names the backend that ran it, on the `backend:` line of its "Starting job container" group.

#### Service containers

A job's `services` are started before its steps run. When a service's image or its `options` declare a healthcheck, the runner waits for it to report healthy, so a workflow does not have to poll for its own services:

```yaml
services:
  postgres:
    image: postgres:17
    options: >-
      --health-cmd pg_isready
      --health-interval 5s
      --health-retries 10
```

A service that reports unhealthy fails the job right away, with its container log. One that never becomes healthy fails it after `container.service_ready_timeout` (default `5m`, negative disables the wait). A service that exits without declaring a healthcheck only gets its log and a warning.

A job in a container reaches a service by its id on the job network, on the port the service listens on, for example `psql -h postgres -p 5432`. The started containers also fill the `job` context: `job.container.{id,network}` and `job.services.<id>.{id,network,ports}`, where `ports` maps a container port to the host port Docker published it on, for the services that publish one.

Unlike GitHub, a job whose steps run on the host (a `host` label without `container:`) starts no service containers, so `job.services` and `job.container` stay empty. Give such a job a `container:` when it needs services.

#### Proxy

Set these variables in the runner's environment, with systemd `Environment=`, `docker run -e`, or Kubernetes `env:`:

```sh
http_proxy=http://proxy.example:3128
https_proxy=http://proxy.example:3128
no_proxy=gitea.internal,.example.local
```

The runner uses them for its own requests and gives them to every job, in lower and upper case.

These hosts are added to `no_proxy` for jobs, so they are always reached directly:

- the cache server
- `localhost`, `127.0.0.1` and `::1`
- the job's service containers
- the Docker daemon, when it is reached over `tcp://`

Gitea is not added. Add it to `no_proxy` yourself if it should be reached directly.

To change a value for one job, set it in a step's `env:` or in the job's `container.env`. Setting it at workflow or job level has no effect. To change it for the whole runner, set it in `runner.envs`. A `no_proxy` set there is added to the list above instead of replacing it.

Images are pulled by the Docker daemon, which needs its own proxy setting. In the `dind` images the daemon runs in the same container and reads the variables above. For any other daemon, see [the Docker documentation](https://docs.docker.com/engine/daemon/proxy/). The runner logs a warning at startup if it has a proxy and the daemon does not.

Dockerfile actions are built with these variables as build arguments, so their `RUN` steps can reach the network.

A password in a proxy URL is hidden in job logs. Any step can still read it, because the step is given the proxy URL in its environment.

#### Caching (`actions/cache`)

Each runner starts its own cache server automatically. Cache entries are local to that runner — runners do not share a cache by default.

**Cache service v2**

`actions/cache@v4.2` and later can use the *cache service v2* API. The runner serves it from the same store as v1, on by default, and it works with `external_server`. Turn it off with:

```yaml
cache:
  v2: false
```

Those actions refuse any host they do not take for GitHub. Rather than misreport the server URL, the runner edits that check out of the action's own bundle and keeps the untouched copy beside it; a bundle it does not recognise is left alone and keeps to v1. The same edit lets the stock `actions/upload-artifact` and `actions/download-artifact` work from `v4.4.0` on, without the `gitea-upload-artifact` fork.

**Shared cache across multiple runners**

Run one dedicated `gitea-runner cache-server` that all runners point at.

1. Create a config file for the cache server host:

   ```yaml
   cache:
     dir: /data/actcache
     port: 8088
     external_secret: "replace-with-a-strong-random-secret"
     # external_secret_file: /path/to/secret # secret can also be passed via a file
   ```

2. Start the server:

   ```bash
   gitea-runner -c cache-server-config.yaml cache-server
   ```

3. On every runner:

   ```yaml
   cache:
     external_server: "http://<cache-server-host>:8088/"
     external_secret: "replace-with-a-strong-random-secret"  # must match the server
     # external_secret_file: /path/to/secret # secret can also be passed via a file
   ```

Jobs reach the cache server at `external_server`, so when a reverse proxy fronts the server, point `external_server` at the proxy. The cache server itself needs no extra configuration.

Alternatively, mount the same NFS/CIFS share on every runner and point `cache.dir` at it — simpler, but with weaker isolation between repositories.

**S3 / MinIO** — mount object storage as a FUSE filesystem (e.g. [s3fs](https://github.com/s3fs-fuse/s3fs-fuse) or [goofys](https://github.com/kahing/goofys)) and set `cache.dir` to the mount point.

Flags `--dir`, `--host`, and `--port` on `cache-server` override the corresponding `cache.*` YAML keys; all other settings, including `external_secret`, require the config file.

#### Official Docker image

Besides `GITEA_INSTANCE_URL` and `GITEA_RUNNER_REGISTRATION_TOKEN`, the image entrypoint supports optional variables such as `CONFIG_FILE` (passed through as `-c`), `GITEA_RUNNER_LABELS`, `GITEA_RUNNER_EPHEMERAL`, `GITEA_RUNNER_ONCE`, `GITEA_RUNNER_NAME`, `GITEA_MAX_REG_ATTEMPTS`, `RUNNER_STATE_FILE`, and `GITEA_RUNNER_REGISTRATION_TOKEN_FILE`. See [scripts/run.sh](scripts/run.sh) for exact behavior.

For a fuller container-oriented walkthrough, see [examples/docker](examples/docker/README.md).

While the runner is idle it cleans up after earlier jobs:
- when `container.bind_workdir` is enabled, stale task workspace directories older than `runner.workdir_cleanup_age` are removed (default: `24h`; set `0` to disable)
- only purely numeric subdirectories under `container.workdir_parent` are treated as task workspaces and may be removed
- cleanup assumes `container.workdir_parent` is not shared across multiple runners
- on runners that use docker, per-job networks left behind by jobs the runner did not live to tear down are removed, identified by the `com.gitea.runner.uuid` label carrying this runner's uuid
- cleanup runs every `runner.idle_cleanup_interval` (default: `10m`; set `0` to disable), and setting either knob to `0` disables all of the above

#### Post-task script (`runner.post_task_script`)

Optional host script that runs **after** each task's built-in cleanup (post-steps, container teardown, bind-workdir removal). Use it for extra machine housekeeping — Docker pruning, disk cleanup, and similar.

**While the script runs, the runner stops task heartbeats and stays offline from Gitea's perspective until the script exits (or hits `runner.post_task_script_timeout`, default `5m`).** A script that blocks without exiting keeps the runner from taking new work for up to that timeout. Script output goes to the runner log, not the job log; a non-zero exit is warned but does not change the job result.

On Windows, use `.exe`, `.bat`, or `.cmd` paths; **PowerShell (`.ps1`) is not supported yet** as the configured path — wrap commands in a `.cmd` file instead.

See **[docs/post-task-script.md](docs/post-task-script.md)** for lifecycle details, environment variables, timeout interaction, and platform notes.

#### Job hooks (`runner.hooks.job_started`, `runner.hooks.job_completed`)

Optional scripts that run **inside the job environment** (the job container, or the host in host mode), before the job's first step and after its last one. They are the equivalent of GitHub's `ACTIONS_RUNNER_HOOK_JOB_STARTED` / `ACTIONS_RUNNER_HOOK_JOB_COMPLETED`, which are read when the settings are unset.

Because they run where the steps run and see the job's environment, they are the place for per-job setup no workflow should have to carry: registry logins, mirror configuration, or masking runner-wide secrets with `::add-mask::`. Their output is part of the job log and is scanned for workflow commands, and they can export to the job through `$GITHUB_ENV` and `$GITHUB_PATH`.

Both hooks are synchronous and block the job while they run. Either one exiting non-zero fails the job, and there is no per-hook timeout.

See **[docs/job-hooks.md](docs/job-hooks.md)** for the execution order, environment, and platform notes.

### Example Deployments

Check out the [examples](examples) directory for sample deployment types.
