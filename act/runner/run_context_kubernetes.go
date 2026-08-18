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
		rc.Env["JOB_CONTAINER_NAME"] = name

		envList := make([]string, 0)
		envList = append(envList, rc.runnerEnvFrom(kubernetes.RunnerContext())...)
		envList = append(envList, fmt.Sprintf("%s=%s", "LANG", "C.UTF-8"))

		ext := container.LinuxContainerEnvironmentExtensions{}
		workdir := ext.ToContainerPath(rc.Config.Workdir)

		services := rc.podServiceInputs(ctx)

		kcfg := rc.Config.Kubernetes
		maxLifetime := rc.Config.ContainerMaxLifetime
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
			Tolerations:            podTolerations(kcfg.Tolerations),
			Resources:              kubernetes.PodResources(kcfg.Resources),
			SecurityContext:        kubernetes.PodSecurityContext(kcfg.SecurityContext),
			Privileged:             rc.Config.Privileged,
			TerminationGracePeriod: kcfg.TerminationGracePeriod,
			SchedulingTimeout:      kcfg.SchedulingTimeout,
			ServiceReadyTimeout:    serviceReadyTimeout(kcfg, rc.Config.ServiceReadyTimeout),
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

		for _, service := range services {
			rc.serviceContainers = append(rc.serviceContainers, &serviceContainer{
				name:      service.Name,
				image:     service.Image,
				container: kubernetes.NewSidecarView(pod, service.Name),
			})
		}

		rc.cleanUpJobContainer = pod.Remove()

		defer printStartJobContainerGroup(ctx, image, name, "pod", backendKubernetes)()

		return common.NewPipelineExecutor(
			pod.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
			pod.Start(false),
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

		services = append(services, kubernetes.ServiceInput{
			Name:  serviceID,
			Image: serviceImage,
			Cmd:   interpolatedCmd,
			Env:   envs,
		})
	}

	return services
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
// are not waited on the way a daemon's service containers are. Unset falls back to the
// docker setting, which is what this backend used before the field was read at all.
func serviceReadyTimeout(kcfg KubernetesConfig, fallback time.Duration) time.Duration {
	if kcfg.ServiceReadyTimeout != 0 {
		return kcfg.ServiceReadyTimeout
	}
	return fallback
}

func podTolerations(tolerations []KubernetesToleration) []kubernetes.Toleration {
	out := make([]kubernetes.Toleration, 0, len(tolerations))
	for _, t := range tolerations {
		out = append(out, kubernetes.Toleration(t))
	}
	return out
}

// errUnsupportedInKubernetes fails fast for functionality that needs a Docker daemon
// job containers don't have under the kubernetes execution backend, such as
// `uses: docker://image` steps or actions with `runs.using: docker`.
func errUnsupportedInKubernetes(what string) error {
	return fmt.Errorf("%s is unsupported in kubernetes execution mode: job containers do not have access to a Docker daemon", what)
}
