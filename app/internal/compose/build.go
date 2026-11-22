package compose

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"stack-launcher.local/app/internal/registry"
)

// GitBuild describes a build rooted in a service repository.
type GitBuild struct {
	Repository, Ref, Context, Dockerfile, Target string
	PrebuiltRepository                           string
	Args                                         []string
}

func extractBuild(service map[string]any) (*GitBuild, error) {
	extension, _ := service["x-launcher"].(map[string]any)
	git, hasGit := extension["git"]
	build, hasBuild := service["build"]
	if hasGit != hasBuild {
		return nil, errors.New("git and build must be supplied together")
	}
	if !hasGit {
		return nil, nil
	}
	delete(service, "build")
	return parseBuild(git, build)
}
func parseBuild(git, value any) (*GitBuild, error) {
	b, err := parseBuildSource(git)
	if err != nil {
		return nil, err
	}
	if err := buildSettings(b, value); err != nil {
		return nil, err
	}
	if invalidBuildPath(b) {
		return nil, errors.New("invalid build path")
	}
	if b.PrebuiltRepository != "" && (filepath.IsAbs(b.Context) || filepath.IsAbs(b.Dockerfile)) {
		return nil, errors.New("build context and dockerfile must be relative paths")
	}
	b.Context = strings.ReplaceAll(b.Context, "$$", "$")
	b.Dockerfile = strings.ReplaceAll(b.Dockerfile, "$$", "$")
	b.Target = strings.ReplaceAll(b.Target, "$$", "$")
	return b, nil
}
func parseBuildSource(git any) (*GitBuild, error) {
	g, err := mapping(git, "git")
	if err != nil {
		return nil, err
	}
	if err = fields(g, "git", "repository", "ref", "prebuilt_repository"); err != nil {
		return nil, err
	}
	repo, ok := stringValue(g["repository"])
	if !ok {
		return nil, errors.New("git repository is required")
	}
	ref, ok := stringValue(g["ref"])
	if !ok {
		return nil, errors.New("git ref is required")
	}
	prebuilt, err := parsePrebuiltRepository(g)
	if err != nil {
		return nil, err
	}
	return &GitBuild{
		Repository: repo, Ref: strings.ReplaceAll(ref, "$$", "$"), PrebuiltRepository: prebuilt,
		Context: ".", Dockerfile: "Dockerfile",
	}, nil
}
func parsePrebuiltRepository(g map[string]any) (string, error) {
	value, exists := g["prebuilt_repository"]
	if !exists {
		return "", nil
	}
	repo, ok := stringValue(value)
	if !ok {
		return "", errors.New("git prebuilt_repository must be a nonempty string")
	}
	if err := registry.ValidateRepository(repo); err != nil {
		return "", fmt.Errorf("git prebuilt_repository: %w", err)
	}
	return repo, nil
}
func buildSettings(b *GitBuild, value any) error {
	switch v := value.(type) {
	case string:
		b.Context = v
		return nil
	case map[string]any:
		return buildMapping(b, v)
	default:
		return errors.New("build must be a string or mapping")
	}
}
func buildMapping(b *GitBuild, v map[string]any) error {
	if err := fields(v, "build", "context", "dockerfile", "args", "target"); err != nil {
		return err
	}
	for key, dest := range map[string]*string{"context": &b.Context, "dockerfile": &b.Dockerfile, "target": &b.Target} {
		if val, exists := v[key]; exists {
			str, valid := stringValue(val)
			if !valid {
				return fmt.Errorf("build %s must be a nonempty string", key)
			}
			*dest = str
		}
	}
	if val, exists := v["args"]; exists {
		var err error
		b.Args, err = buildArgs(val)
		return err
	}
	return nil
}
func buildArgs(value any) ([]string, error) {
	var args []string
	switch v := value.(type) {
	case map[string]any:
		var err error
		args, err = buildArgMap(v)
		if err != nil {
			return nil, err
		}
	case []any:
		var err error
		args, err = buildArgList(v)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("build args must be a mapping or list")
	}
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "$$", "$")
	}
	return args, nil
}

func buildArgMap(v map[string]any) ([]string, error) {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		val, ok := v[k].(string)
		if !ok || k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(val, 0) {
			return nil, errors.New("build args must explicitly assign strings")
		}
		args = append(args, k+"="+val)
	}
	return args, nil
}
func buildArgList(v []any) ([]string, error) {
	var args []string
	for _, item := range v {
		arg, ok := item.(string)
		if !ok || !strings.Contains(arg, "=") || strings.HasPrefix(arg, "=") || strings.ContainsRune(arg, 0) {
			return nil, errors.New("build args must explicitly assign strings")
		}
		args = append(args, arg)
	}
	return args, nil
}

func invalidBuildPath(b *GitBuild) bool {
	return b.Context == "" || strings.ContainsRune(b.Context, 0) || strings.ContainsRune(b.Dockerfile, 0)
}
