// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import "time"

// ServiceHealthCheck is the readiness a workflow declares for a service container, through
// the `--health-*` options the docker backend reads. It is expressed without docker's own
// types so a backend that has no daemon can translate it into whatever its runtime uses.
type ServiceHealthCheck struct {
	// Test is docker's form: {"CMD-SHELL", script} or {"CMD", argv...}.
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	StartPeriod time.Duration
	Retries     int
}

// Declared reports whether the service gave anything to wait on.
func (c ServiceHealthCheck) Declared() bool { return len(c.Test) > 0 }

// ParseServiceHealthCheck reads the health options out of a service container's `options:`
// string. A service that declares no healthcheck, or disables it, yields an undeclared value
// rather than an error: there is simply nothing to wait for.
func ParseServiceHealthCheck(options string) (ServiceHealthCheck, error) {
	if options == "" {
		return ServiceHealthCheck{}, nil
	}

	_, copts, _, err := parseContainerOptions(options)
	if err != nil {
		return ServiceHealthCheck{}, err
	}
	if copts.noHealthcheck || copts.healthCmd == "" {
		return ServiceHealthCheck{}, nil
	}

	return ServiceHealthCheck{
		Test:        []string{"CMD-SHELL", copts.healthCmd},
		Interval:    copts.healthInterval,
		Timeout:     copts.healthTimeout,
		StartPeriod: copts.healthStartPeriod,
		Retries:     copts.healthRetries,
	}, nil
}
