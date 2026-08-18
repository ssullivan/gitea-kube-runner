// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package labels

import (
	"errors"
	"strings"
)

const (
	SchemeHost       = "host"
	SchemeDocker     = "docker"
	SchemeKubernetes = "kubernetes"
)

type Label struct {
	Name   string
	Schema string
	Arg    string
	// Opaque marks a label whose name contains a colon but no supported schema,
	// like "pool:e57e18d4-...". It is kept verbatim and behaves like a host label.
	Opaque bool
}

func Parse(str string) (*Label, error) {
	if str == "" {
		return nil, errors.New("empty label")
	}

	splits := strings.SplitN(str, ":", 3)
	label := &Label{
		Name:   splits[0],
		Schema: SchemeHost,
		Arg:    "",
	}
	if len(splits) >= 2 {
		label.Schema = splits[1]
	}
	if len(splits) >= 3 {
		label.Arg = splits[2]
	}
	if label.Schema != SchemeHost && label.Schema != SchemeDocker && label.Schema != SchemeKubernetes {
		// Not a schema we know: the colon belongs to the label name itself.
		return &Label{
			Name:   str,
			Schema: SchemeHost,
			Opaque: true,
		}, nil
	}
	return label, nil
}

type Labels []*Label

func (l Labels) RequireDocker() bool {
	for _, label := range l {
		if label.Schema == SchemeDocker {
			return true
		}
	}
	return false
}

func (l Labels) RequireKubernetes() bool {
	for _, label := range l {
		if label.Schema == SchemeKubernetes {
			return true
		}
	}
	return false
}

// schemePrefixed marks an image with the backend that runs it. An argument-less label
// ("ubuntu-latest:docker") keeps producing the empty string it always did, because a bare
// "docker://" is non-empty and would stop runsOnImage falling through to Config.Platforms,
// which is where such a label's image has always come from.
func schemePrefixed(scheme, arg string) string {
	image := strings.TrimPrefix(arg, "//")
	if image == "" {
		return ""
	}
	return scheme + image
}

// PickPlatform resolves a job's runs-on values against this runner's labels. The
// result is always one of: "docker://<image>", "kubernetes://<image>", the host-mode
// marker "-self-hosted", or "" for a label that names no image — callers that need the
// bare image must strip the scheme prefix themselves, since the prefix is what lets them
// tell backends apart.
func (l Labels) PickPlatform(runsOn []string) string {
	platforms := make(map[string]string, len(l))
	for _, label := range l {
		switch label.Schema {
		case SchemeDocker:
			// "//" will be ignored
			platforms[label.Name] = schemePrefixed("docker://", label.Arg)
		case SchemeKubernetes:
			platforms[label.Name] = schemePrefixed("kubernetes://", label.Arg)
		case SchemeHost:
			platforms[label.Name] = "-self-hosted"
		default:
			// unreachable: Parse only produces host, docker, or kubernetes schemas
			continue
		}
	}
	for _, v := range runsOn {
		if v, ok := platforms[v]; ok {
			return v
		}
	}

	// TODO: support multiple labels
	// like:
	//   ["ubuntu-22.04"] => "ubuntu:22.04"
	//   ["with-gpu"] => "linux:with-gpu"
	//   ["ubuntu-22.04", "with-gpu"] => "ubuntu:22.04_with-gpu"

	// return default.
	// So the runner receives a task with a label that the runner doesn't have,
	// it happens when the user have edited the label of the runner in the web UI.
	// TODO: it may be not correct, what if the runner is used as host mode only?
	return "docker.gitea.com/runner-images:ubuntu-latest"
}

func (l Labels) Names() []string {
	names := make([]string, 0, len(l))
	for _, label := range l {
		names = append(names, label.Name)
	}
	return names
}

func (l Labels) ToStrings() []string {
	ls := make([]string, 0, len(l))
	for _, label := range l {
		lbl := label.Name
		if !label.Opaque && label.Schema != "" {
			lbl += ":" + label.Schema
			if label.Arg != "" {
				lbl += ":" + label.Arg
			}
		}
		ls = append(ls, lbl)
	}
	return ls
}
