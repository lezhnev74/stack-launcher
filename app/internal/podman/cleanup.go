package podman

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func failedLaunch(r runner, args []string, project string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := cleanupProject(ctx, r, args, project)
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func cleanupProject(ctx context.Context, r runner, args []string, project string) error {
	started := time.Now()
	r.logf("Running Compose down project=%q", project)
	// Pass --volumes to Podman rm to remove image-created anonymous volumes.
	// Compose down's --volumes flag would also remove the named volumes.
	// The caller bounds down so an operator stop can honor stop_grace_period.
	_, downErr := r.runCommand(ctx, append(args, "--podman-rm-args=--volumes", "down")...)
	r.logf("Compose down finished succeeded=%t elapsed=%s, verifying project cleanup", downErr == nil, time.Since(started))
	ids, psErr := projectContainers(ctx, r, project)
	networks, networkErr := r.run(ctx, "network", "ls", "-q", "--filter", "label=com.docker.compose.project="+project)
	if len(ids) > 0 {
		psErr = errors.Join(psErr, fmt.Errorf("%d project containers remain", len(ids)))
	}
	if strings.TrimSpace(networks) != "" {
		networkErr = errors.Join(networkErr, errors.New("project networks remain"))
	}
	if err := errors.Join(downErr, psErr, networkErr); err != nil {
		return fmt.Errorf("cleanup project %q: %w", project, err)
	}
	r.logf("Project cleanup verified, named volumes retained elapsed=%s", time.Since(started))
	return nil
}
