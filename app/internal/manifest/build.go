package manifest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const buildGitTimeout = 2 * time.Minute

// ResolveBuildCommit pins a service build ref without fetching its source tree.
func ResolveBuildCommit(ctx context.Context, stateDir, repo, ref string) (string, error) {
	if !validBuildSource(repo, ref) {
		return "", errors.New("invalid repository or ref")
	}
	ctx, cancel := context.WithTimeout(ctx, buildGitTimeout)
	defer cancel()
	_, commit, err := resolveBuildRef(ctx, gitRunner{stateDir: stateDir}, buildRemote(repo), ref)
	return commit, err
}

// CheckoutBuild creates a transient checkout for a service build.
func CheckoutBuild(ctx context.Context, stateDir, repo, ref string) (string, string, error) {
	if !validBuildSource(repo, ref) {
		return "", "", errors.New("invalid repository or ref")
	}
	ctx, cancel := context.WithTimeout(ctx, buildGitTimeout)
	defer cancel()
	run := gitRunner{stateDir: stateDir}
	remote := buildRemote(repo)
	oid, _, err := resolveBuildRef(ctx, run, remote, ref)
	if err != nil {
		return "", "", err
	}
	dir, err := newCheckout(stateDir)
	if err != nil {
		return "", "", err
	}
	commit, err := checkoutBuild(ctx, run, dir, remote, oid)
	if err != nil {
		return "", "", errors.Join(err, os.RemoveAll(dir))
	}
	return dir, commit, nil
}
func buildRemote(repo string) string {
	return "ssh://git@github.com/" + strings.ToLower(repo) + ".git"
}

// resolveBuildRef returns the advertised object and its peeled revision. A tag
// object can be fetched directly, while a prebuilt image is keyed by its target.
func resolveBuildRef(ctx context.Context, run runner, remote, ref string) (object, commit string, err error) {
	if commitPattern.MatchString(ref) {
		oid := strings.ToLower(ref)
		return oid, oid, nil
	}
	output, err := run.run(ctx, "", "ls-remote", "--heads", "--tags", remote)
	if err != nil {
		return "", "", err
	}
	refs := advertisedRefs(output)
	object = refs[ref]
	if object == "" || strings.HasSuffix(ref, "^{}") {
		return "", "", errors.New("ref is missing or ambiguous. Use a full commit SHA")
	}
	commit = refs[ref+"^{}"]
	if commit == "" {
		commit = object
	}
	return object, commit, nil
}
func checkoutBuild(ctx context.Context, run gitRunner, dir, remote, oid string) (string, error) {
	commit, err := checkoutGit(ctx, run, dir, remote, oid)
	if err != nil {
		return "", err
	}
	if err = os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("remove git metadata: %w", err)
	}
	return commit, nil
}
func checkoutGit(ctx context.Context, run runner, dir, remote, oid string) (string, error) {
	for _, args := range [][]string{{"init", "--quiet", "--template="}, {"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--depth=1", remote, oid}, {"checkout", "--quiet", "--detach", "FETCH_HEAD"}} {
		if _, err := run.run(ctx, dir, args...); err != nil {
			return "", err
		}
	}
	commit, err := run.run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(commit)), nil
}

func validBuildSource(repo, ref string) bool {
	return repoPattern.MatchString(repo) && validParts(strings.Split(repo, "/")) && ref != "" && !strings.ContainsAny(ref, "\x00\n\r")
}
