package compose

import (
	"errors"
	"fmt"
	"strings"

	"stack-launcher.local/app/internal/ledger"
)

func prepareServices(services, resources map[string]any, endpoint Endpoint, ctx pathContext, env ledger.Environment, stopping bool) error {
	for name, v := range services {
		service, err := mapping(v, "service "+name)
		if err != nil {
			return err
		}
		if err = serviceReferences(service, resources); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if err = prepareService(name, service, endpoint, ctx, env[name], stopping); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
	}
	return nil
}
func prepareService(name string, s map[string]any, endpoint Endpoint, ctx pathContext, env map[string]string, stopping bool) error {
	if err := validateService(s); err != nil {
		return err
	}
	if err := prepareServiceEnvironment(s, env, ctx.stateDir, stopping); err != nil {
		return err
	}
	if err := prepareMounts(s, ctx); err != nil {
		return err
	}
	return preparePorts(s, endpoint.portFor(name))
}
func validateService(s map[string]any) error {
	if err := fields(s, "service", "image", "pull_policy", "build", "command", "entrypoint", "environment", "env_file", "depends_on", "healthcheck", "ports", "volumes", "networks", "configs", "secrets", "restart", "working_dir", "user", "init", "stop_grace_period", "expose", "labels", "x-launcher"); err != nil {
		return err
	}
	if _, ok := s["build"]; !ok {
		if _, ok := stringValue(s["image"]); !ok {
			return errors.New("image is required")
		}
	}
	if err := validateLabels(s["labels"]); err != nil {
		return err
	}
	return serviceInputs(s)
}
func serviceInputs(s map[string]any) error {
	if v, ok := s["environment"]; ok && v == nil {
		return errors.New("environment must be a mapping or list")
	}
	return environment(s["environment"])
}
func environment(v any) error {
	if v == nil {
		return nil
	}
	switch env := v.(type) {
	case map[string]any:
		return environmentMap(env)
	case []any:
		return environmentList(env)
	default:
		return errors.New("environment must be a mapping or list")
	}
}
func environmentMap(env map[string]any) error {
	for _, value := range env {
		switch value.(type) {
		case string, bool, int, float64:
		default:
			return errors.New("environment values must be explicit scalars")
		}
	}
	return nil
}
func environmentList(env []any) error {
	for _, value := range env {
		s, ok := value.(string)
		if !ok || !strings.Contains(s, "=") || strings.HasPrefix(s, "=") {
			return errors.New("environment entries must explicitly assign values")
		}
	}
	return nil
}

func serviceReferences(s, resources map[string]any) error {
	for _, kind := range []string{"configs", "secrets"} {
		v, ok := s[kind]
		if !ok {
			continue
		}
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s must be a list", kind)
		}
		for _, item := range list {
			if err := serviceReference(kind, item, resources[kind]); err != nil {
				return err
			}
		}
	}
	return nil
}
func serviceReference(kind string, v, declarations any) error {
	name, err := referenceSource(kind, v)
	if err != nil {
		return err
	}
	declared, _ := declarations.(map[string]any)
	if _, ok := declared[name]; !ok {
		return fmt.Errorf("%s source %q is not declared", kind, name)
	}
	return nil
}
func referenceSource(kind string, v any) (string, error) {
	switch ref := v.(type) {
	case string:
		if volumeName.MatchString(ref) {
			return ref, nil
		}
	case map[string]any:
		if err := fields(ref, kind, "source", "target", "uid", "gid", "mode"); err != nil {
			return "", err
		}
		if name, ok := ref["source"].(string); ok && volumeName.MatchString(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s source must be a valid name", kind)
}

func validateLabels(value any) error {
	var keys []string
	switch labels := value.(type) {
	case nil:
		return nil
	case map[string]any:
		for key := range labels {
			keys = append(keys, key)
		}
	case []any:
		keys = labelKeys(labels)
	default:
		return errors.New("labels must be a mapping or list")
	}
	return checkLabelKeys(keys)
}

func checkLabelKeys(keys []string) error {
	for _, key := range keys {
		if strings.HasPrefix(key, "io.podman.compose.") || strings.HasPrefix(key, "com.docker.compose.") {
			return fmt.Errorf("reserved Compose label %q", key)
		}
	}
	return nil
}

func labelKeys(labels []any) []string {
	var keys []string
	for _, value := range labels {
		if label, ok := value.(string); ok {
			key, _, _ := strings.Cut(label, "=")
			keys = append(keys, key)
		}
	}
	return keys
}
