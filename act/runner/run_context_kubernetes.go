// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	maps0 "maps"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/act/container/kubernetes"
)

// newPodEnvironment is a variable so tests can substitute an environment that needs no
// cluster, mirroring newContainer for the docker backend.
var newPodEnvironment = func(cfg kubernetes.Config, input *kubernetes.PodInput) (*kubernetes.PodEnvironment, error) {
	client, err := kubernetes.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewPodEnvironment(client, input), nil
}

// startPodEnvironment runs the job in its own Kubernetes Pod, with one sidecar per
// service container. Unlike the docker backend there is no per-job network to create:
// the containers of a Pod already share one, and services are reached on localhost.
func (rc *RunContext) startPodEnvironment() common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		image := rc.platformImage(ctx)
		rawLogger := logger.WithField(rawOutputField, true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof("%s", s)
			} else {
				rawLogger.Debugf("%s", s)
			}
			return true
		})

		name := rc.jobContainerName()
		// The name the Pod is actually created under, not the docker-style one it is derived
		// from: a step addressing the workload by this would otherwise get NotFound.
		rc.Env["JOB_CONTAINER_NAME"] = kubernetes.PodName(name)

		kcfg := rc.Config.Kubernetes

		envList := make([]string, 0)
		envList = append(envList, rc.runnerEnvFrom(kubernetes.RunnerContext(kcfg.NodeSelector))...)
		envList = append(envList, fmt.Sprintf("%s=%s", "LANG", "C.UTF-8"))

		ext := container.LinuxContainerEnvironmentExtensions{}
		workdir := ext.ToContainerPath(rc.Config.Workdir)

		if err := rc.checkUnsupportedCredentials(); err != nil {
			return err
		}
		rc.warnUnsupportedJobContainer(ctx)

		services := rc.podServiceInputs(ctx)

		maxLifetime := rc.Config.ContainerMaxLifetime
		// Resolved once and shared with the orchestration around the backend, so
		// kubernetes.service_ready_timeout governs both waits rather than only the inner one.
		readyTimeout := serviceReadyTimeout(kcfg, rc.Config.ServiceReadyTimeout)
		rc.serviceReadyTimeout = readyTimeout
		input := &kubernetes.PodInput{
			Name:  name,
			Image: image,
			// One value drives both, so the process the job runs in and the deadline the
			// cluster enforces on its Pod cannot drift apart.
			Entrypoint:  []string{"/bin/sleep", fmt.Sprint(maxLifetime.Round(time.Second).Seconds())},
			MaxLifetime: maxLifetime,
			// Nil unless the runner is itself a Pod sharing this namespace, in which case
			// the cluster collects job Pods it dies without deleting.
			Owner:      kubernetes.OwnerFromEnv(),
			WorkingDir: workdir,
			Env:        envList,
			Stdout:     logWriter,
			Stderr:     logWriter,
			// The one emptyDir is mounted at each of these, so the workspace, the
			// runner's act path and the tool cache all exist for the job's steps.
			MountPaths:             []string{workdir, ext.GetActPath(), rc.toolCache(container.DefaultToolCache)},
			AllocatePTY:            rc.Config.AllocatePTY,
			Services:               services,
			Namespace:              kcfg.Namespace,
			ServiceAccountName:     kcfg.ServiceAccountName,
			ImagePullSecrets:       kcfg.ImagePullSecrets,
			ImagePullPolicy:        imagePullPolicy(kcfg.ImagePullPolicy, rc.Config.ForcePull),
			Labels:                 kcfg.PodLabels,
			Annotations:            kcfg.PodAnnotations,
			NodeSelector:           kcfg.NodeSelector,
			Tolerations:            kcfg.Tolerations,
			Resources:              kcfg.Resources,
			SecurityContext:        kcfg.SecurityContext,
			Privileged:             rc.Config.Privileged,
			TerminationGracePeriod: kcfg.TerminationGracePeriod,
			SchedulingTimeout:      kcfg.SchedulingTimeout,
			ServiceReadyTimeout:    readyTimeout,
		}

		pod, err := newPodEnvironment(kubernetes.Config{
			Namespace:         kcfg.Namespace,
			Kubeconfig:        kcfg.Kubeconfig,
			KubeconfigContext: kcfg.KubeconfigContext,
		}, input)
		if err != nil {
			return err
		}
		rc.JobContainer = pod

		// Read back from the input: the container names are assigned there, and a sidecar
		// view addressing a service by anything else would look up a container that the Pod
		// spec never named.
		for _, service := range input.Services {
			rc.serviceContainers = append(rc.serviceContainers, &serviceContainer{
				name:      service.Name,
				image:     service.Image,
				container: kubernetes.NewSidecarView(pod, service.ContainerName),
			})
		}

		rc.cleanUpJobContainer = pod.Remove()

		defer printStartJobContainerGroup(ctx, image, kubernetes.PodName(name), "pod", backendKubernetes)()

		return common.NewPipelineExecutor(
			pod.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
			pod.Start(false),
			// The sidecar views exist so the service orchestration drives both backends;
			// skipping these left svc.info unset, so `job.services.*` resolved to nothing
			// and a sidecar that died on startup never had its logs read.
			rc.reportUnstartedServices(),
			rc.waitForServiceContainers(),
			rc.captureJobContainerInfo(),
			pod.Copy(pod.GetActPath()+"/", &container.FileEntry{
				Name: "workflow/event.json",
				Mode: 0o644,
				Body: rc.EventJSON,
			}, &container.FileEntry{
				Name: "workflow/envs.txt",
				Mode: 0o666,
				Body: "",
			}),
		)(ctx)
	}
}

// podServiceInputs interpolates the job's services into sidecar specs. Service
// volumes, ports and options have no equivalent here: a sidecar shares the Pod's
// network namespace, so its ports are already reachable on localhost.
func (rc *RunContext) podServiceInputs(ctx context.Context) []kubernetes.ServiceInput {
	logger := common.Logger(ctx)

	var services []kubernetes.ServiceInput
	for serviceID, spec := range rc.Run.Job().Services {
		serviceImage := rc.ExprEval.Interpolate(ctx, spec.Image)
		if serviceImage == "" {
			logger.Infof("The service '%s' will not be started because the container definition has an empty image.", serviceID)
			continue
		}

		interpolatedEnvs := make(map[string]string, len(spec.Env)+len(rc.Config.ProxyEnv))
		maps0.Copy(interpolatedEnvs, rc.Config.ProxyEnv)
		for k, v := range spec.Env {
			interpolatedEnvs[k] = rc.ExprEval.Interpolate(ctx, v)
		}
		envs := make([]string, 0, len(interpolatedEnvs))
		for k, v := range interpolatedEnvs {
			envs = append(envs, fmt.Sprintf("%s=%s", k, v))
		}

		interpolatedCmd := make([]string, 0, len(spec.Cmd))
		for _, v := range spec.Cmd {
			interpolatedCmd = append(interpolatedCmd, rc.ExprEval.Interpolate(ctx, v))
		}

		if len(spec.Volumes) > 0 {
			logger.Warnf("The service '%s' declares volumes, which the kubernetes backend does not support; they are ignored.", serviceID)
		}

		// A sidecar with no probe is ready the instant it starts, so without this the job's
		// steps run against a service that may not be listening yet.
		health, err := container.ParseServiceHealthCheck(rc.ExprEval.Interpolate(ctx, spec.Options))
		if err != nil {
			logger.Warnf("The service '%s' has options the runner could not read, so its readiness is not waited for: %v", serviceID, err)
		} else if !health.Declared() {
			logger.Warnf("The service '%s' declares no --health-cmd, so its steps start as soon as the container does rather than when it is ready.", serviceID)
		}

		services = append(services, kubernetes.ServiceInput{
			Name:        serviceID,
			Image:       serviceImage,
			Cmd:         interpolatedCmd,
			Env:         envs,
			HealthCheck: health,
		})
	}

	return services
}

// checkUnsupportedCredentials fails a job that configured registry credentials, which this
// backend cannot honour: the kubelet pulls the image, so credentials have to reach it as an
// imagePullSecret. Ignoring them silently left the Pod in ImagePullBackOff with nothing to
// say the configured credentials had never been read.
func (rc *RunContext) checkUnsupportedCredentials() error {
	if c := rc.Run.Job().Container(); c != nil && len(c.Credentials) > 0 {
		return errUnsupportedInKubernetes("the job container's `credentials:`")
	}
	for serviceID, spec := range rc.Run.Job().Services {
		if len(spec.Credentials) > 0 {
			return errUnsupportedInKubernetes(fmt.Sprintf("the service %q `credentials:`", serviceID))
		}
	}
	return nil
}

// warnUnsupportedJobContainer says what the Pod spec has no place for, rather than leaving a
// job to fail later for reasons that look nothing like the setting that was ignored.
func (rc *RunContext) warnUnsupportedJobContainer(ctx context.Context) {
	logger := common.Logger(ctx)

	// The runner's own container.options reaches every job on the docker backend, so a label
	// moved to kubernetes drops it for the jobs that declare no container: of their own —
	// which is most of them, and the case the checks below never see.
	if rc.Config.ContainerOptions != "" {
		logger.Warnf("The runner's container.options are not supported by the kubernetes backend and are ignored; use the kubernetes: section instead.")
	}

	c := rc.Run.Job().Container()
	if c == nil {
		return
	}
	if c.Options != "" {
		logger.Warnf("The job container's options are not supported by the kubernetes backend and are ignored; use the kubernetes: section instead.")
	}
	if len(c.Volumes) > 0 {
		logger.Warnf("The job container declares volumes, which the kubernetes backend does not support; they are ignored.")
	}
	if len(c.Ports) > 0 {
		logger.Warnf("The job container declares ports, which need no publishing in a Pod; they are ignored.")
	}
}

// imagePullPolicy lets force_pull ask for a re-pull the way it does for docker, since
// the kubelet decides on its own otherwise. An explicit policy always wins.
func imagePullPolicy(configured string, forcePull bool) string {
	if configured != "" {
		return configured
	}
	if forcePull {
		return "Always"
	}
	return ""
}

// serviceReadyTimeout lets the kubernetes section set its own, since sidecars sharing a Pod
// are not waited on the way a daemon's service containers are. Unset falls back to the docker
// setting, and unset there to the same default the docker backend applies: zero has to become
// a real bound, or a sidecar wedged in ContainerCreating holds the job until the task deadline
// with nothing to cut it short. Negative still means do not wait at all.
func serviceReadyTimeout(kcfg KubernetesConfig, fallback time.Duration) time.Duration {
	if kcfg.ServiceReadyTimeout != 0 {
		return kcfg.ServiceReadyTimeout
	}
	if fallback != 0 {
		return fallback
	}
	return defaultServiceReadyTimeout
}

// errUnsupportedInKubernetes fails fast for functionality that needs a Docker daemon
// job containers don't have under the kubernetes execution backend, such as
// `uses: docker://image` steps or actions with `runs.using: docker`.
func errUnsupportedInKubernetes(what string) error {
	return fmt.Errorf("%s is unsupported in kubernetes execution mode: job containers do not have access to a Docker daemon", what)
}
