package podman

import (
	"context"
	"fmt"
	"strings"

	"stack-launcher.local/app/internal/compose"
)

// Remove deletes resources bearing this stack's Compose project label.
func Remove(ctx context.Context, stackID string) error {
	r := runner{env: cleanEnv("")}
	if err := checkRootless(ctx, r); err != nil {
		return err
	}
	filter := "label=com.docker.compose.project=" + compose.ProjectName(stackID)
	if err := removeContainers(ctx, r, filter); err != nil {
		return err
	}
	for _, kind := range []string{"network", "volume"} {
		if err := removeLabeled(ctx, r, kind, filter); err != nil {
			return err
		}
	}
	return nil
}

func removeContainers(ctx context.Context, r runner, filter string) error {
	out, err := r.run(ctx, "ps", "-aq", "--no-trunc", "--sort", "created", "--filter", filter)
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	// Podman requires a dependency to exist before creating its dependent.
	for i := len(ids) - 1; i >= 0; i-- {
		if _, err = r.run(ctx, "rm", "--force", "--ignore", "--volumes", "--", ids[i]); err != nil {
			return fmt.Errorf("remove container: %w", err)
		}
	}
	return nil
}

func removeLabeled(ctx context.Context, r runner, kind, filter string) error {
	out, err := r.run(ctx, kind, "ls", "-q", "--filter", filter)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(out) {
		if _, err = r.run(ctx, kind, "rm", "--", id); err != nil {
			return fmt.Errorf("remove %s: %w", kind, err)
		}
	}
	return nil
}
