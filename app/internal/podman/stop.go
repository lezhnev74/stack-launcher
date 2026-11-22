package podman

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/runlog"
)

// Stop removes a project's containers and networks while retaining named volumes.
func Stop(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string) error {
	runlog.Printf(stackID, "Preparing saved Compose project for teardown")
	prepared, images, err := savedProject(ctx, cfg, db, stackID)
	if err != nil {
		return err
	}
	pinned, err := prepared.PinImages(images)
	if err != nil {
		return fmt.Errorf("pin images: %w", err)
	}
	r, err := localRunner(ctx, cfg, stackID)
	if err != nil {
		return err
	}
	r.logf("Writing private Compose file for teardown project=%q", prepared.ProjectName)
	file, err := writeComposeFile(cfg.StateDir, pinned)
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	defer func() { _ = os.RemoveAll(dir) }()
	r.dir = dir
	return cleanupProject(ctx, r, composeArgs(prepared.ProjectName, file), prepared.ProjectName)
}

func savedProject(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string) (compose.Prepared, map[string]ledger.Image, error) {
	manifest, err := db.GetManifest(ctx, stackID)
	if err != nil {
		return compose.Prepared{}, nil, err
	}
	images, err := db.GetImages(ctx, stackID)
	if err != nil {
		return compose.Prepared{}, nil, err
	}
	prepared, err := compose.PrepareForStop(cfg.StateDir, stackID, manifest, imageReferences(images))
	if err != nil {
		return compose.Prepared{}, nil, fmt.Errorf("prepare stack: %w", err)
	}
	return prepared, images, nil
}

func imageReferences(images map[string]ledger.Image) map[string]string {
	refs := make(map[string]string, len(images))
	for name, image := range images {
		if image.Repository == "" {
			refs[name] = image.Reference
		}
	}
	return refs
}
