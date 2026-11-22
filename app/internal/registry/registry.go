// Package registry resolves prebuilt images using the Podman registry configuration.
package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/docker/distribution/registry/api/errcode"
	"github.com/docker/distribution/registry/api/v2"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/sysregistriesv2"
	"go.podman.io/image/v5/types"
)

// ErrNotFound means the registry reported that the requested manifest or repository is missing.
var ErrNotFound = errors.New("prebuilt image not found")

// ValidateRepository requires an explicit registry and repository without a tag or digest.
func ValidateRepository(repository string) error {
	_, err := parseRepository(repository)
	return err
}

func parseRepository(repository string) (reference.Named, error) {
	domain, _, hasPath := strings.Cut(repository, "/")
	if !hasPath || (domain != "localhost" && !strings.ContainsAny(domain, ".:")) {
		return nil, errors.New("must include the registry host and repository path")
	}
	name, err := reference.ParseNormalizedNamed(repository)
	if err != nil {
		return nil, err
	}
	if !reference.IsNameOnly(name) {
		return nil, errors.New("repository must not have a tag or digest")
	}
	if !validRegistryPort(domain) {
		return nil, errors.New("invalid registry port")
	}
	return name, nil
}

func validRegistryPort(domain string) bool {
	_, port, hasPort := strings.Cut(domain, ":")
	if !hasPort {
		return true
	}
	number, err := strconv.ParseUint(port, 10, 16)
	return err == nil && number > 0
}

// ResolveDigest returns the computed digest of the manifest for the requested tag.
// An empty auth file uses the launcher account's default Podman credential lookup.
func ResolveDigest(ctx context.Context, repository, tag, authFile string) (string, error) {
	name, err := parseRepository(repository)
	if err != nil {
		return "", err
	}
	tagged, err := reference.WithTag(name, tag)
	if err != nil {
		return "", err
	}
	ref, err := docker.NewReference(tagged)
	if err != nil {
		return "", err
	}
	if err := checkAuthFile(authFile); err != nil {
		return "", err
	}
	// Podman reads this configuration in a fresh process for each pull.
	sysregistriesv2.InvalidateCache()
	return fetchManifestDigest(ctx, ref, &types.SystemContext{AuthFilePath: authFile})
}

func checkAuthFile(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("access registry auth file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("registry auth file is not a regular file")
	}
	return nil
}

func fetchManifestDigest(ctx context.Context, ref types.ImageReference, sys *types.SystemContext) (string, error) {
	source, err := ref.NewImageSource(ctx, sys)
	if err != nil {
		return "", classifyLookupError(ctx, err)
	}
	defer func() { _ = source.Close() }()
	blob, _, err := source.GetManifest(ctx, nil)
	if err != nil {
		return "", err
	}
	if manifest.GuessMIMEType(blob) == "" {
		return "", errors.New("registry returned an unrecognized image manifest")
	}
	value, err := manifest.Digest(blob)
	return value.String(), err
}

func classifyLookupError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.Join(err, ctx.Err())
	}
	var coded errcode.ErrorCoder
	if errors.As(err, &coded) {
		switch coded.ErrorCode() {
		case v2.ErrorCodeManifestUnknown, v2.ErrorCodeNameUnknown:
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}
	return err
}
