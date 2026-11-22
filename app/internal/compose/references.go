package compose

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"stack-launcher.local/app/internal/ledger"
)

// ServiceReference describes a service's source and ports without resolving host files.
type ServiceReference struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Value      string   `json:"value"`
	Repository string   `json:"repository,omitempty"`
	Ports      []int    `json:"ports,omitempty"`
	Exposed    []string `json:"exposed,omitempty"`
	HTTPPort   int      `json:"http_port,omitempty"`
	HTTPS      bool     `json:"https,omitempty"`
}

// TemplateServices derives the services and refs shown in the configuration editor.
func TemplateServices(manifest ledger.Manifest) ([]ServiceReference, error) {
	root, err := decode(manifest.Content)
	if err != nil {
		return nil, err
	}
	if err = applyOverrides(root, manifest.References, nil); err != nil {
		return nil, err
	}
	services, endpoint, err := validateDocument(root)
	if err != nil {
		return nil, err
	}
	refs := make([]ServiceReference, 0, len(services))
	for _, name := range slices.Sorted(maps.Keys(services)) {
		ref, err := describeTemplateService(name, services[name], endpoint)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func describeTemplateService(name string, value any, endpoint Endpoint) (ServiceReference, error) {
	service, err := mapping(value, "service")
	if err != nil {
		return ServiceReference{}, err
	}
	port := endpoint.portFor(name)
	if err := preparePorts(service, port); err != nil {
		return ServiceReference{}, err
	}
	build, err := extractBuild(service)
	if err != nil {
		return ServiceReference{}, err
	}
	ref := ServiceReference{
		Name: name, Ports: containerPorts(service), Exposed: exposedPorts(service),
		HTTPPort: port, HTTPS: port != 0 && endpoint.HTTPS,
	}
	if build != nil {
		ref.Kind, ref.Value, ref.Repository = "git", build.Ref, build.Repository
	} else {
		ref.Kind = "image"
		ref.Value, _ = service["image"].(string)
	}
	return ref, nil
}

func exposedPorts(service map[string]any) []string {
	entries, _ := service["expose"].([]any)
	var ports []string
	for _, entry := range entries {
		switch port := entry.(type) {
		case int:
			ports = append(ports, strconv.Itoa(port))
		case string:
			ports = append(ports, port)
		}
	}
	return ports
}

func applyServiceReferences(services map[string]any, refs map[string]ledger.ReferenceOverride) error {
	for name, ref := range refs {
		if err := setServiceReference(name, services[name], ref); err != nil {
			return fmt.Errorf("reference override for service %q: %w", name, err)
		}
	}
	return nil
}

func setServiceReference(name string, value any, ref ledger.ReferenceOverride) error {
	service, err := mapping(value, "service")
	if err != nil {
		return err
	}
	switch ref.Kind {
	case "image":
		return setImageOverride(name, service, ref.Value)
	case "git":
		extension, _ := service["x-launcher"].(map[string]any)
		git, err := mapping(extension["git"], "git")
		if err != nil {
			return err
		}
		git["ref"] = strings.ReplaceAll(ref.Value, "$", "$$")
		return nil
	default:
		return fmt.Errorf("unsupported source %q", ref.Kind)
	}
}
