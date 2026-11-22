package compose

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
	"stack-launcher.local/app/internal/ledger"
)

func applyOverrides(root map[string]any, refs map[string]ledger.ReferenceOverride, overrides map[string]string) error {
	services, err := mapping(root["services"], "services")
	if err != nil {
		return err
	}
	if err := applyServiceReferences(services, refs); err != nil {
		return err
	}
	for name, image := range overrides {
		value, ok := services[name]
		if !ok {
			return fmt.Errorf("image override for unknown service %q", name)
		}
		if err := setImageOverride(name, value, image); err != nil {
			return err
		}
	}
	return nil
}

func setImageOverride(name string, value any, image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image override for service %q is blank", name)
	}
	service, err := mapping(value, "service "+name)
	if err != nil {
		return err
	}
	extension, _ := service["x-launcher"].(map[string]any)
	if _, hasGit := extension["git"]; hasGit {
		return fmt.Errorf("image override for Git service %q is unsupported", name)
	}
	service["image"] = image
	return nil
}

func hasCompletionDependency(services map[string]any) bool {
	for _, v := range services {
		s := v.(map[string]any)
		deps, _ := s["depends_on"].(map[string]any)
		for _, v := range deps {
			options, _ := v.(map[string]any)
			if options["condition"] == "service_completed_successfully" {
				return true
			}
		}
	}
	return false
}

func describeServices(services map[string]any) (map[string]Service, error) {
	result := make(map[string]Service, len(services))
	for name, v := range services {
		s := v.(map[string]any)
		build, err := extractBuild(s)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		policy, err := pullPolicy(s, build)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		image, _ := s["image"].(string)
		result[name] = Service{Image: image, PullPolicy: policy, Build: build, Ports: containerPorts(s)}
	}
	return result, nil
}

func pullPolicy(service map[string]any, build *GitBuild) (string, error) {
	value, exists := service["pull_policy"]
	if !exists {
		return "", nil
	}
	if build != nil {
		return "", errors.New("pull_policy requires a registry image")
	}
	switch value {
	case "always":
		return "", nil
	case "newer":
		return "newer", nil
	default:
		return "", errors.New("pull_policy must be always or newer")
	}
}

var imageID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PinImages produces a provider-specific copy using the selected image IDs.
func (p Prepared) PinImages(images map[string]ledger.Image) ([]byte, error) {
	root, err := decode(p.YAML)
	if err != nil {
		return nil, err
	}
	services, err := mapping(root["services"], "services")
	if err != nil {
		return nil, err
	}
	if err := pinServices(services, images, root); err != nil {
		return nil, err
	}
	delete(root, "configs")
	relabelSecrets(root)
	output, err := yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode Compose: %w", err)
	}
	return output, nil
}

func pinServices(services map[string]any, images map[string]ledger.Image, root map[string]any) error {
	if len(images) != len(services) {
		return errors.New("image ownership must cover every service")
	}
	for name, value := range services {
		if err := pinService(name, value, images, root); err != nil {
			return err
		}
	}
	return nil
}

func pinService(name string, value any, images map[string]ledger.Image, root map[string]any) error {
	image, ok := images[name]
	if !ok || !imageID.MatchString(image.ID) {
		return fmt.Errorf("invalid image ID for service %q", name)
	}
	service, err := mapping(value, "service "+name)
	if err != nil {
		return err
	}
	service["image"] = "sha256:" + image.ID
	service["pull_policy"] = "never"
	if err := lowerConfigs(service, root); err != nil {
		return err
	}
	relabelBinds(service)
	return nil
}

func lowerConfigs(service, root map[string]any) error {
	refs, _ := service["configs"].([]any)
	declarations, _ := root["configs"].(map[string]any)
	volumes, _ := service["volumes"].([]any)
	for _, ref := range refs {
		mount, err := configMount(ref, declarations)
		if err != nil {
			return err
		}
		volumes = append(volumes, mount)
	}
	if len(refs) > 0 {
		service["volumes"] = volumes
		delete(service, "configs")
	}
	return nil
}

func configMount(ref any, declarations map[string]any) (map[string]any, error) {
	name, err := referenceSource("configs", ref)
	if err != nil {
		return nil, err
	}
	declaration, ok := declarations[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config %q is missing", name)
	}
	source, ok := declaration["file"].(string)
	if !ok {
		return nil, fmt.Errorf("config %q file is missing", name)
	}
	target, err := configTarget(ref, name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "bind", "source": source, "target": target, "read_only": true}, nil
}

func configTarget(ref any, name string) (string, error) {
	details, ok := ref.(map[string]any)
	if !ok {
		return "/" + name, nil
	}
	value, exists := details["target"]
	if !exists {
		return "/" + name, nil
	}
	target, valid := value.(string)
	if !valid || !strings.HasPrefix(target, "/") {
		return "", errors.New("config target must be absolute")
	}
	return target, nil
}

func relabelBinds(service map[string]any) {
	volumes, _ := service["volumes"].([]any)
	for _, value := range volumes {
		mount, _ := value.(map[string]any)
		if mount["type"] == "bind" {
			mount["bind"] = map[string]any{"selinux": "z"}
		}
	}
}

func relabelSecrets(root map[string]any) {
	secrets, _ := root["secrets"].(map[string]any)
	for _, value := range secrets {
		secret, ok := value.(map[string]any)
		if ok {
			secret["x-podman.relabel"] = "z"
		}
	}
}
