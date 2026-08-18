// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/act/filecollector"

	"github.com/go-git/go-billy/v5/helper/polyfill"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// Kubernetes has no copy subresource: `kubectl cp` is tar piped over exec, and so is
// this. It needs `tar` in the job image, the same implicit requirement `docker cp` has.

// copyTarStream extracts a tar stream into destPath inside the job container.
func (e *PodEnvironment) copyTarStream(ctx context.Context, destPath string, tarStream io.Reader) error {
	var stderr bytes.Buffer
	err := e.exec(ctx, execOptions{
		container: JobContainerName,
		command:   []string{"sh", "-c", `mkdir -p "$0" && exec tar -xf - -C "$0"`, destPath},
		stdin:     tarStream,
		stderr:    &stderr,
	})
	if err != nil {
		return fmt.Errorf("copy to %s in pod %s: %w: %s", destPath, e.podName, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// getContainerArchive streams srcPath out of the job container as a tar archive, the
// reverse of copyTarStream and the shape parse-env-file style callers expect.
func (e *PodEnvironment) getContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	dir, base := path.Split(strings.TrimSuffix(srcPath, "/"))
	if dir == "" {
		dir = "/"
	}
	if base == "" {
		base = "."
	}

	reader, writer := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		err := e.exec(ctx, execOptions{
			container: JobContainerName,
			command:   []string{"tar", "-cf", "-", "-C", dir, base},
			stdout:    drainedPipeWriter{writer},
			stderr:    &stderr,
		})
		if err != nil {
			err = fmt.Errorf("read %s from pod %s: %w: %s", srcPath, e.podName, err, strings.TrimSpace(stderr.String()))
		}
		_ = writer.CloseWithError(err)
	}()

	return reader, nil
}

// drainedPipeWriter discards what is written after the consumer closes the read end,
// which is normal rather than exceptional: parseEnvFile takes the first tar entry and
// closes. Reporting the short write instead would make the stream copy inside client-go
// log an error after every step.
type drainedPipeWriter struct{ w *io.PipeWriter }

func (d drainedPipeWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if errors.Is(err, io.ErrClosedPipe) {
		return len(p), nil
	}
	return n, err
}

// copyContent tars the given files and extracts them at destPath, mirroring the docker
// backend's Copy.
func (e *PodEnvironment) copyContent(ctx context.Context, destPath string, files ...*container.FileEntry) error {
	logger := common.Logger(ctx)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, file := range files {
		logger.Debugf("Writing entry to tarball %s len:%d", file.Name, len(file.Body))
		if err := tw.WriteHeader(&tar.Header{
			Name: file.Name,
			Mode: file.Mode,
			Size: int64(len(file.Body)),
		}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(file.Body)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}

	return e.copyTarStream(ctx, destPath, &buf)
}

// copyDir tars a host directory and extracts it in the job container. The tar entries
// carry their full destination paths, so it is extracted at the root, as the docker
// backend does.
func (e *PodEnvironment) copyDir(ctx context.Context, dstPath, srcPath string, useGitIgnore bool) error {
	logger := common.Logger(ctx)

	tarFile, err := os.CreateTemp("", "act")
	if err != nil {
		return err
	}
	defer func() {
		name := tarFile.Name()
		if err := tarFile.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			logger.Error(err)
		}
		if err := os.Remove(name); err != nil {
			logger.Error(err)
		}
	}()

	logger.Debugf("Writing tarball %s from %s", tarFile.Name(), srcPath)
	tw := tar.NewWriter(tarFile)

	srcPrefix := filepath.Dir(srcPath)
	if !strings.HasSuffix(srcPrefix, string(filepath.Separator)) {
		srcPrefix += string(filepath.Separator)
	}

	var ignorer gitignore.Matcher
	if useGitIgnore {
		ps, err := gitignore.ReadPatterns(polyfill.New(osfs.New(srcPath)), nil)
		if err != nil {
			logger.Debugf("Error loading .gitignore: %v", err)
		}
		ignorer = gitignore.NewMatcher(ps)
	}

	fc := &filecollector.FileCollector{
		Fs:        &filecollector.DefaultFs{},
		Ignorer:   ignorer,
		SrcPath:   srcPath,
		SrcPrefix: srcPrefix,
		Handler: &filecollector.TarCollector{
			TarWriter: tw,
			DstDir:    dstPath[1:],
		},
	}

	if err := filepath.Walk(srcPath, fc.CollectFiles(ctx, []string{})); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := tarFile.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek tar archive: %w", err)
	}

	logger.Debugf("Extracting content from '%s' to '%s'", tarFile.Name(), dstPath)
	return e.copyTarStream(ctx, "/", tarFile)
}
