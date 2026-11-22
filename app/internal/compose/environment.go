package compose

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"stack-launcher.local/app/internal/ledger"
)

// ValidateEnvironment validates the shape and literal values of operator overrides.
func ValidateEnvironment(env ledger.Environment) error {
	for service, values := range env {
		if service == "" || strings.TrimSpace(service) != service || values == nil {
			return errors.New("invalid environment service")
		}
		if err := validateEnvValues(values); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvValues(values map[string]string) error {
	for key, value := range values {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return errors.New("invalid environment variable")
		}
	}
	return nil
}
func checkEnvServices(services map[string]any, env ledger.Environment) error {
	for name := range env {
		if _, ok := services[name]; !ok {
			return fmt.Errorf("unknown environment service %q", name)
		}
	}
	return nil
}

// ValidateEnvironmentServices checks service names without resolving admin files.
func ValidateEnvironmentServices(manifest ledger.Manifest, env ledger.Environment) error {
	services, err := manifestServices(manifest)
	if err != nil {
		return err
	}
	return checkEnvServices(services, env)
}

// ServiceNames lists the services declared by a saved manifest.
func ServiceNames(manifest ledger.Manifest) ([]string, error) {
	services, err := manifestServices(manifest)
	return slices.Sorted(maps.Keys(services)), err
}

func manifestServices(manifest ledger.Manifest) (map[string]any, error) {
	root, err := decode(manifest.Content)
	if err != nil {
		return nil, err
	}
	return mapping(root["services"], "services")
}

func validateRuntimeOverrides(services map[string]any, env ledger.Environment) error {
	if err := ValidateEnvironment(env); err != nil {
		return err
	}
	return checkEnvServices(services, env)
}
func prepareServiceEnvironment(service map[string]any, env map[string]string, stateDir string, stopping bool) error {
	if stopping {
		delete(service, "env_file")
		return nil
	}
	if err := resolveEnvFiles(service, stateDir); err != nil {
		return err
	}
	mergeEnvironment(service, env)
	return nil
}
func resolveEnvFiles(service map[string]any, stateDir string) error {
	value, ok := service["env_file"]
	if !ok {
		return nil
	}
	files, err := envFileList(value)
	if err != nil {
		return err
	}
	for i, item := range files {
		name, ok := item.(string)
		if !ok {
			return errors.New("env_file must contain filenames")
		}
		path, err := envPath(stateDir, name)
		if err != nil {
			return fmt.Errorf("env_file %q: %w", name, err)
		}
		files[i] = strings.ReplaceAll(path, "$", "$$")
	}
	service["env_file"] = files
	return nil
}
func envFileList(value any) ([]any, error) {
	switch v := value.(type) {
	case string:
		return []any{v}, nil
	case []any:
		return v, nil
	default:
		return nil, errors.New("env_file must be a filename or list")
	}
}
func envPath(stateDir, name string) (string, error) {
	if !validEnvName(name) {
		return "", errors.New("filename must be a literal basename")
	}
	root, err := envRoot(stateDir)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return "", err
	}
	if err := contained(root, path); err != nil {
		return "", errors.New("file escapes env directory")
	}
	if err := readableEnvFile(path); err != nil {
		return "", err
	}
	return path, nil
}
func validEnvName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, "$\\")
}
func envRoot(stateDir string) (string, error) {
	state, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return "", err
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return "", err
	}
	root := filepath.Join(state, "env")
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("env must be a real directory")
	}
	return root, nil
}
func readableEnvFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
		return errors.New("file must be regular and readable")
	}
	file, err := os.Open(path) // #nosec G304 -- resolved and checked inside the admin directory
	if err != nil {
		return err
	}
	return file.Close()
}
func mergeEnvironment(service map[string]any, overrides map[string]string) {
	if len(overrides) == 0 {
		return
	}
	values := environmentValues(service["environment"])
	for key := range values {
		// Compare the name the provider will see after Compose dollar unescaping.
		if _, replaced := overrides[strings.ReplaceAll(key, "$$", "$")]; replaced {
			delete(values, key)
		}
	}
	for key, value := range overrides {
		values[strings.ReplaceAll(key, "$", "$$")] = strings.ReplaceAll(value, "$", "$$")
	}
	service["environment"] = values
}

func environmentValues(value any) map[string]any {
	values := map[string]any{}
	switch v := value.(type) {
	case map[string]any:
		return v
	case []any:
		for _, item := range v {
			key, val, _ := strings.Cut(item.(string), "=")
			values[key] = val
		}
	}
	return values
}

func hasEnvFiles(services map[string]any) bool {
	for _, value := range services {
		if service, ok := value.(map[string]any); ok {
			if _, ok := service["env_file"]; ok {
				return true
			}
		}
	}
	return false
}
