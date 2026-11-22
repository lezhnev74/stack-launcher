// Package podman launches validated Compose projects using a local rootless Podman.
package podman

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/registry"
	"stack-launcher.local/app/internal/runlog"
)

// Result describes the verified host bindings of a launched project.
type Result struct {
	ProjectName  string
	Endpoint     netip.AddrPort
	HTTPS        bool
	Bindings     map[string]map[int]netip.AddrPort
	containerIDs map[string]string
}

// Launch pulls or builds and pins images, then starts the project and waits for readiness.
func Launch(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string, overrides map[string]string) (Result, error) {
	return launch(ctx, cfg, db, stackID, overrides, registry.ResolveDigest)
}

func launch(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string, overrides map[string]string, resolveDigest digestResolver) (Result, error) {
	runlog.Printf(stackID, "Preparing saved Compose manifest for launch")
	prepared, err := prepareLaunch(ctx, cfg, db, stackID, overrides)
	if err != nil {
		return Result{}, err
	}
	reportProgress(ctx, "Checking local rootless Podman.")
	r, err := localRunner(ctx, cfg, stackID)
	if err != nil {
		return Result{}, err
	}
	r.logf("Preparing service images services=%d project=%q", len(prepared.Services), prepared.ProjectName)
	images, err := prepareImages(ctx, r, cfg.StateDir, cfg.RegistryAuthFile, prepared.Services, resolveDigest)
	if err != nil {
		return Result{}, err
	}
	r.logf("Pinning selected service images")
	reportProgress(ctx, "Pinning service images.")
	pinned, err := prepared.PinImages(images)
	if err != nil {
		return Result{}, fmt.Errorf("pin images: %w", err)
	}
	return launchPinned(ctx, cfg.StateDir, r, prepared, images, pinned, func() error {
		return db.SaveImages(ctx, stackID, images)
	})
}

// Start recreates a project from its saved manifest and immutable image identities.
func Start(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string) (Result, error) {
	return startPinned(ctx, cfg, db, stackID, false)
}

// RetryPinned clears a failed project's resources and runs its launch commands again.
func RetryPinned(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string) (Result, error) {
	runlog.Printf(stackID, "Cleaning up previous launch before retry")
	reportProgress(ctx, "Cleaning up containers and networks from the previous attempt.")
	cleanupCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if err := Stop(cleanupCtx, cfg, db, stackID); err != nil {
		return Result{}, fmt.Errorf("clean up previous launch: %w", err)
	}
	return startPinned(ctx, cfg, db, stackID, true)
}

func startPinned(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string, runLaunchCommands bool) (Result, error) {
	runlog.Printf(stackID, "Loading pinned service images")
	images, err := db.GetImages(ctx, stackID)
	if err != nil {
		return Result{}, err
	}
	runlog.Printf(stackID, "Preparing saved Compose manifest with pinned images services=%d", len(images))
	prepared, err := prepareProject(ctx, cfg, db, stackID, imageReferences(images))
	if err != nil {
		return Result{}, err
	}
	pinned, err := prepared.PinImages(images)
	if err != nil {
		return Result{}, fmt.Errorf("pin images: %w", err)
	}
	r, err := localRunner(ctx, cfg, stackID)
	if err != nil {
		return Result{}, err
	}
	if !runLaunchCommands {
		r.logf("Skipping fresh-launch commands for start")
		prepared.LaunchCommands = nil
	}
	return launchPinned(ctx, cfg.StateDir, r, prepared, images, pinned, nil)
}

func prepareLaunch(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string, overrides map[string]string) (compose.Prepared, error) {
	prepared, err := prepareProject(ctx, cfg, db, stackID, overrides)
	if err != nil {
		return compose.Prepared{}, err
	}
	return prepared, checkImageOwnership(ctx, db, stackID)
}

func prepareProject(ctx context.Context, cfg config.Config, db *ledger.Ledger, stackID string, overrides map[string]string) (compose.Prepared, error) {
	manifest, err := db.GetManifest(ctx, stackID)
	if err != nil {
		return compose.Prepared{}, err
	}
	env, err := db.GetEnvironment(ctx, stackID)
	if err != nil {
		return compose.Prepared{}, err
	}
	prepared, err := compose.PrepareRuntime(cfg.StateDir, stackID, manifest, overrides, env)
	if err != nil {
		return compose.Prepared{}, fmt.Errorf("prepare stack: %w", err)
	}
	if prepared.HasCompletionDependency {
		return compose.Prepared{}, errors.New("prepare stack: podman-compose cannot enforce the service_completed_successfully dependency condition")
	}
	return prepared, nil
}

func checkImageOwnership(ctx context.Context, db *ledger.Ledger, stackID string) error {
	_, err := db.GetImages(ctx, stackID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("stack %q already has saved images; launch has already been claimed", stackID)
}

func localRunner(ctx context.Context, cfg config.Config, stackID string) (runner, error) {
	runlog.Printf(stackID, "Resolving Compose provider")
	r, err := newRunner(cfg)
	if err != nil {
		return r, err
	}
	r.runID = stackID
	r.logf("Checking local rootless Podman")
	return r, checkRootless(ctx, r)
}

func newRunner(cfg config.Config) (runner, error) {
	provider := cfg.ComposeProvider
	if provider == "" {
		provider = config.DefaultComposeProvider
	}
	path, err := exec.LookPath(provider)
	if err != nil {
		return runner{}, fmt.Errorf("resolve Compose provider %q: %w", provider, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return runner{}, fmt.Errorf("resolve Compose provider: %w", err)
	}
	return runner{env: cleanEnv(path)}, nil
}
func checkRootless(ctx context.Context, r runner) error {
	out, err := r.run(ctx, "info", "--format", "{{.Host.ServiceIsRemote}} {{.Host.Security.Rootless}}")
	if err != nil {
		return fmt.Errorf("check local rootless Podman: %w", err)
	}
	if out != "false true" {
		return errors.New("default Podman engine must be local and rootless")
	}
	return nil
}

var fullID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validReference(ref string) bool {
	return ref != "" && strings.TrimSpace(ref) == ref && !strings.ContainsAny(ref, " \t\r\n") && !strings.Contains(ref, "://")
}

type registryImage struct {
	reference string
	policy    string
}

func prepareImages(ctx context.Context, r runner, stateDir, auth string, services map[string]compose.Service, resolveDigest digestResolver) (map[string]ledger.Image, error) {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	images := make(map[string]ledger.Image, len(names))
	cache := map[registryImage]string{}
	for _, name := range names {
		started := time.Now()
		image, err := prepareServiceImage(ctx, r, stateDir, auth, name, services[name], cache, resolveDigest)
		if err != nil {
			return nil, err
		}
		images[name] = image
		r.logf("Service image prepared service=%q image_id=%q git_commit=%q elapsed=%s", name, image.ID, image.Commit, time.Since(started))
	}
	return images, nil
}
func prepareServiceImage(ctx context.Context, r runner, stateDir, auth, name string, service compose.Service, cache map[registryImage]string, resolveDigest digestResolver) (ledger.Image, error) {
	if service.Build != nil {
		return prepareGitImage(ctx, r, stateDir, auth, name, service.Build, cache, resolveDigest)
	}
	reportProgress(ctx, "Preparing the registry image for %q.", name)
	ref := service.Image
	if err := validateImageReference(ref, name); err != nil {
		return ledger.Image{}, err
	}
	id, err := cachedPull(ctx, r, auth, registryImage{reference: ref, policy: service.PullPolicy}, name, cache)
	if err != nil {
		return ledger.Image{}, err
	}
	return ledger.Image{Reference: ref, ID: id}, nil
}
func pullImage(ctx context.Context, r runner, auth string, image registryImage) (string, error) {
	args := []string{"pull", "--quiet"}
	if image.policy != "" {
		args = append(args, "--policy="+image.policy)
	}
	if auth != "" {
		args = append(args, "--authfile", auth)
	}
	args = append(args, "--", "docker://"+image.reference)
	id, err := r.run(ctx, args...)
	if err != nil {
		return "", err
	}
	id = strings.TrimPrefix(id, "sha256:")
	if !fullID.MatchString(id) {
		return "", errors.New("pull did not return a full lowercase image ID")
	}
	return id, nil
}
func launchPinned(ctx context.Context, stateDir string, r runner, prepared compose.Prepared, images map[string]ledger.Image, pinned []byte, claimImages func() error) (Result, error) {
	r.logf("Writing private Compose file project=%q", prepared.ProjectName)
	file, err := writeComposeFile(stateDir, pinned)
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Dir(file)
	defer func() { _ = os.RemoveAll(dir) }()
	if claimImages != nil {
		r.logf("Saving pinned image identities")
		if err := claimImages(); err != nil {
			return Result{}, err
		}
	}
	r.dir = dir
	r.redact = prepared.HasEnvFiles
	args := composeArgs(prepared.ProjectName, file)
	result, err := startAndCheck(ctx, r, args, prepared, images)
	if err != nil {
		r.logf("Project startup failed, beginning cleanup error=%q", err)
		reportProgress(ctx, "Startup checks failed. Cleaning up the Compose project.")
		return Result{}, failedLaunch(r, args, prepared.ProjectName, err)
	}
	return result, nil
}

func startAndCheck(ctx context.Context, r runner, args []string, prepared compose.Prepared, images map[string]ledger.Image) (Result, error) {
	started := time.Now()
	reportProgress(ctx, "Starting containers with Podman Compose.")
	r.logf("Starting Compose project project=%q timeout=%s", prepared.ProjectName, commandTimeout)
	if _, err := r.run(ctx, append(args, "up", "-d")...); err != nil {
		return Result{}, fmt.Errorf("start project %q: %w", prepared.ProjectName, err)
	}
	r.logf("Compose up finished elapsed=%s", time.Since(started))
	reportProgress(ctx, "Checking container state, health, and port bindings.")
	result, err := waitReady(ctx, r, prepared, images)
	if err != nil {
		return Result{}, err
	}
	if len(prepared.LaunchCommands) == 0 {
		return result, nil
	}
	if err := runLaunchCommands(ctx, r, prepared.LaunchCommands, result.containerIDs); err != nil {
		return Result{}, err
	}
	r.logf("Checking readiness again after fresh-launch commands")
	reportProgress(ctx, "Checking readiness again after launch commands.")
	return waitReady(ctx, r, prepared, images)
}

func runLaunchCommands(ctx context.Context, r runner, commands []compose.LaunchCommand, ids map[string]string) error {
	for _, command := range commands {
		reportProgress(ctx, "Running a launch command for %q.", command.Service)
		args := append([]string{"exec", "--", ids[command.Service]}, command.Args...)
		started := time.Now()
		r.logf("Running fresh-launch command service=%q timeout=%s", command.Service, commandTimeout)
		if _, err := r.run(ctx, args...); err != nil {
			return fmt.Errorf("launch command for service %q: %w", command.Service, err)
		}
		r.logf("Fresh-launch command finished service=%q elapsed=%s", command.Service, time.Since(started))
	}
	return nil
}

func writeComposeFile(stateDir string, pinned []byte) (string, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	dir, err := os.MkdirTemp(stateDir, ".compose-*")
	if err != nil {
		return "", fmt.Errorf("create private Compose directory: %w", err)
	}
	file := filepath.Join(dir, "compose.yaml")
	if err = os.WriteFile(file, pinned, 0600); err != nil {
		return "", fmt.Errorf("write private Compose file: %w", errors.Join(err, os.RemoveAll(dir)))
	}
	return file, nil
}

func composeArgs(project, file string) []string {
	return []string{"compose", "--in-pod=false", "--env-file", "/dev/null", "--project-name", project, "--file", file}
}

func validateImageReference(ref, name string) error {
	if !validReference(ref) {
		return fmt.Errorf("service %q: invalid registry image reference %q", name, ref)
	}
	return nil
}

func cachedPull(ctx context.Context, r runner, auth string, image registryImage, name string, cache map[registryImage]string) (string, error) {
	if id, ok := cache[image]; ok {
		r.logf("Reusing image already pulled for this run service=%q image=%q", name, image.reference)
		return id, nil
	}
	r.logf("Pulling service image service=%q image=%q policy=%q", name, image.reference, image.policy)
	id, err := pullImage(ctx, r, auth, image)
	if err != nil {
		return "", fmt.Errorf("pull image %q for service %q: %w", image.reference, name, err)
	}
	cache[image] = id
	return id, nil
}
