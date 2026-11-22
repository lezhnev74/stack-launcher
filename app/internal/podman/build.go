package podman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/manifest"
	"stack-launcher.local/app/internal/registry"
)

type digestResolver func(ctx context.Context, repository, tag, authFile string) (string, error)

func prepareGitImage(ctx context.Context, r runner, stateDir, auth, name string, build *compose.GitBuild, cache map[registryImage]string, resolveDigest digestResolver) (ledger.Image, error) {
	repository := build.PrebuiltRepository
	if repository == "" {
		return buildServiceImage(ctx, r, stateDir, auth, name, build, "")
	}
	reportProgress(ctx, "Looking up a prebuilt Git image for %q.", name)
	r.logf("Resolving Git ref for prebuilt image service=%q ref=%q", name, build.Ref)
	commit, err := manifest.ResolveBuildCommit(ctx, stateDir, build.Repository, build.Ref)
	if err != nil {
		return ledger.Image{}, fmt.Errorf("resolve Git ref %q for service %q: %w", build.Ref, name, err)
	}
	tag := "git-" + commit
	r.logf("Looking up prebuilt image service=%q repository=%q tag=%q", name, repository, tag)
	lookupCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	digest, err := resolveDigest(lookupCtx, repository, tag, auth)
	cancel()
	if errors.Is(err, registry.ErrNotFound) {
		r.logf("Prebuilt image missing, building locally service=%q git_commit=%q", name, commit)
		return buildServiceImage(ctx, r, stateDir, auth, name, build, commit)
	}
	if err != nil {
		return ledger.Image{}, fmt.Errorf("lookup prebuilt image %q for service %q: %w", repository+":"+tag, name, err)
	}
	ref := repository + "@" + digest
	reportProgress(ctx, "Preparing the prebuilt image for %q.", name)
	id, err := cachedPull(ctx, r, auth, registryImage{reference: ref}, name, cache)
	if err != nil {
		return ledger.Image{}, err
	}
	return ledger.Image{Reference: ref, ID: id, Repository: strings.ToLower(build.Repository), Commit: commit}, nil
}

func buildServiceImage(ctx context.Context, r runner, stateDir, auth, name string, build *compose.GitBuild, expectedCommit string) (ledger.Image, error) {
	reportProgress(ctx, "Checking out Git source and building the image for %q.", name)
	r.logf("Checking out Git source for local build service=%q ref=%q", name, build.Ref)
	image, err := buildImage(ctx, r, stateDir, auth, build, expectedCommit)
	if err != nil {
		return ledger.Image{}, fmt.Errorf("build image for service %q: %w", name, err)
	}
	return image, nil
}

func buildImage(ctx context.Context, r runner, stateDir, auth string, build *compose.GitBuild, expectedCommit string) (ledger.Image, error) {
	dir, commit, err := checkoutImageSource(ctx, stateDir, build, expectedCommit)
	if err != nil {
		return ledger.Image{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	contextPath, dockerfile, err := buildPaths(dir, build)
	if err != nil {
		return ledger.Image{}, err
	}
	scratch, err := os.MkdirTemp(filepath.Join(stateDir, "checkouts"), "checkout-")
	if err != nil {
		return ledger.Image{}, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	iid, err := filepath.Abs(filepath.Join(scratch, "iid"))
	if err != nil {
		return ledger.Image{}, err
	}
	r.logf("Building local image git_commit=%q", commit)
	id, err := runImageBuild(ctx, r, buildArguments(iid, dockerfile, contextPath, auth, build), iid)
	if err != nil {
		return ledger.Image{}, err
	}
	return ledger.Image{ID: id, Repository: strings.ToLower(build.Repository), Commit: commit}, nil
}

func checkoutImageSource(ctx context.Context, stateDir string, build *compose.GitBuild, expectedCommit string) (string, string, error) {
	ref := build.Ref
	if expectedCommit != "" {
		ref = expectedCommit
	}
	dir, commit, err := manifest.CheckoutBuild(ctx, stateDir, build.Repository, ref)
	if err != nil {
		return "", "", fmt.Errorf("checkout: %w", err)
	}
	if expectedCommit != "" && commit != expectedCommit {
		return "", "", errors.Join(fmt.Errorf("checkout returned commit %s, expected %s", commit, expectedCommit), os.RemoveAll(dir))
	}
	return dir, commit, nil
}
func runImageBuild(ctx context.Context, r runner, args []string, iid string) (string, error) {
	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := r.runBuild(buildCtx, args...); err != nil {
		return "", err
	}
	return readImageID(iid)
}
func buildPaths(dir string, build *compose.GitBuild) (string, string, error) {
	contextPath, err := buildPath(dir, dir, build.Context, true)
	if err != nil {
		return "", "", fmt.Errorf("build context: %w", err)
	}
	dockerfile, err := buildPath(dir, contextPath, build.Dockerfile, false)
	if err != nil {
		return "", "", fmt.Errorf("dockerfile: %w", err)
	}
	if strings.HasSuffix(dockerfile, ".in") {
		return "", "", errors.New("dockerfile .in preprocessing is unsupported")
	}
	return contextPath, dockerfile, nil
}
func buildArguments(iid, dockerfile, contextPath, auth string, build *compose.GitBuild) []string {
	args := []string{"build", "--layers", "--force-rm", "--iidfile", iid, "--file", dockerfile}
	if auth != "" {
		args = append(args, "--authfile", auth)
	}
	if build.Target != "" {
		args = append(args, "--target", build.Target)
	}
	for _, arg := range build.Args {
		args = append(args, "--build-arg", arg)
	}
	args = append(args, "--", contextPath)
	return args
}
func readImageID(iid string) (string, error) {
	data, err := os.ReadFile(iid) // #nosec G304 -- iid is in a private launcher-created directory
	if err != nil {
		return "", err
	}
	id := strings.TrimPrefix(strings.TrimSpace(string(data)), "sha256:")
	if !fullID.MatchString(id) {
		return "", errors.New("build did not return a full lowercase image ID")
	}
	return id, nil
}
func buildPath(root, base, path string, directory bool) (string, error) {
	if err := relativeBuildPath(path); err != nil {
		return "", err
	}
	resolved, err := resolveBuildPath(root, base, path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if wrongKind(info, directory) {
		return "", errors.New("path has incorrect kind")
	}
	return resolved, nil
}

func escapesRoot(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func wrongKind(info os.FileInfo, directory bool) bool {
	return directory && !info.IsDir() || !directory && !info.Mode().IsRegular()
}

func escaped(err error, rel string) bool { return err != nil || escapesRoot(rel) }

func relativeBuildPath(path string) error {
	if filepath.IsAbs(path) {
		return errors.New("absolute paths are unsupported")
	}
	return nil
}

func resolveBuildPath(root, base, path string) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, path))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if escaped(err, rel) {
		return "", errors.New("path escapes repository")
	}
	return resolved, nil
}
