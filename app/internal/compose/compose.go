// Package compose prepares an isolated Compose launch copy without side effects.
package compose

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
	"stack-launcher.local/app/internal/ledger"
)

// Endpoint identifies the container port to route after the project starts.
type Endpoint struct {
	Service string
	Port    int
	HTTPS   bool
}

func (e Endpoint) portFor(service string) int {
	if e.Service == service {
		return e.Port
	}
	return 0
}

// Prepared contains the generated Compose document and its project identity.
type Prepared struct {
	YAML                    []byte
	ProjectName             string
	Endpoint                Endpoint
	Services                map[string]Service
	HasCompletionDependency bool
	HasEnvFiles             bool
	LaunchCommands          []LaunchCommand
}

// Service describes expected runtime state for a Compose service.
type Service struct {
	Image      string
	PullPolicy string
	Build      *GitBuild
	Ports      []int
}

// Prepare validates a saved manifest and returns an in-memory launch copy.
func Prepare(stateDir, stackID string, manifest ledger.Manifest) (Prepared, error) {
	return PrepareWithImages(stateDir, stackID, manifest, nil)
}

// PrepareWithImages applies caller image overrides before validating the launch copy.
func PrepareWithImages(stateDir, stackID string, manifest ledger.Manifest, overrides map[string]string) (Prepared, error) {
	return prepareRuntime(stateDir, stackID, manifest, overrides, nil, false)
}

// PrepareRuntime applies saved environment overrides.
func PrepareRuntime(stateDir, stackID string, manifest ledger.Manifest, images map[string]string, env ledger.Environment) (Prepared, error) {
	return prepareRuntime(stateDir, stackID, manifest, images, env, false)
}

// PrepareForStop reconstructs project metadata without resolving admin files.
func PrepareForStop(stateDir, stackID string, manifest ledger.Manifest, images map[string]string) (Prepared, error) {
	return prepareRuntime(stateDir, stackID, manifest, images, nil, true)
}

// ValidateTemplate checks the document and endpoint before storing a reusable template.
func ValidateTemplate(content []byte) error {
	root, err := decode(content)
	if err != nil {
		return err
	}
	_, _, err = validateDocument(root)
	return err
}
func prepareRuntime(stateDir, stackID string, manifest ledger.Manifest, overrides map[string]string, env ledger.Environment, stopping bool) (Prepared, error) {
	if stackID == "" {
		return Prepared{}, errors.New("stack ID is required")
	}
	root, endpoint, err := prepareDocument(stateDir, manifest, overrides, env, stopping)
	if err != nil {
		return Prepared{}, err
	}
	services := root["services"].(map[string]any)
	described, err := describeServices(services)
	if err != nil {
		return Prepared{}, err
	}
	commands, err := extractLaunchCommands(services)
	if err != nil {
		return Prepared{}, err
	}
	name := ProjectName(stackID)
	root["name"] = name
	delete(root, "x-launcher")
	output, err := yaml.Marshal(root)
	if err != nil {
		return Prepared{}, fmt.Errorf("encode Compose: %w", err)
	}
	return Prepared{
		YAML: output, ProjectName: name, Endpoint: endpoint,
		LaunchCommands: commands, Services: described, HasCompletionDependency: hasCompletionDependency(services), HasEnvFiles: hasEnvFiles(services),
	}, nil
}
func prepareDocument(stateDir string, manifest ledger.Manifest, overrides map[string]string, env ledger.Environment, stopping bool) (map[string]any, Endpoint, error) {
	root, err := decode(manifest.Content)
	if err != nil {
		return nil, Endpoint{}, err
	}
	if err := applyOverrides(root, manifest.References, overrides); err != nil {
		return nil, Endpoint{}, err
	}
	services, endpoint, err := validateDocument(root)
	if err != nil {
		return nil, Endpoint{}, err
	}
	if err = validateRuntimeOverrides(services, env); err != nil {
		return nil, Endpoint{}, err
	}
	ctx := pathContext{stateDir: stateDir, manifest: manifest}
	if err = prepareResources(root, ctx); err != nil {
		return nil, Endpoint{}, err
	}
	if err = prepareServices(services, root, endpoint, ctx, env, stopping); err != nil {
		return nil, Endpoint{}, err
	}
	return root, endpoint, nil
}
func validateDocument(root map[string]any) (map[string]any, Endpoint, error) {
	if err := noInterpolation(root); err != nil {
		return nil, Endpoint{}, err
	}
	if err := fields(root, "Compose", "name", "services", "x-launcher", "volumes", "networks", "configs", "secrets"); err != nil {
		return nil, Endpoint{}, err
	}
	services, err := mapping(root["services"], "services")
	if err != nil || len(services) == 0 {
		return nil, Endpoint{}, errors.New("services must be a nonempty mapping")
	}
	endpoint, err := parseEndpoint(root["x-launcher"], services)
	return services, endpoint, err
}

func decode(content []byte) (map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode Compose: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("compose must contain exactly one document")
	}
	root, err := mapping(value, "Compose")
	if err != nil || len(root) == 0 {
		return nil, errors.New("compose must be a nonempty mapping")
	}
	return root, nil
}
func mapping(value any, label string) (map[string]any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a mapping", label)
	}
	return m, nil
}
func fields(m map[string]any, label string, allowed ...string) error {
	for key := range m {
		found := false
		for _, a := range allowed {
			if key == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: unsupported field %q", label, key)
		}
	}
	return nil
}
func noInterpolation(v any) error {
	switch x := v.(type) {
	case string:
		if interpolation(x) {
			return errors.New("compose interpolation is unsupported; use literal values or $$")
		}
	case map[string]any:
		return checkMap(x)
	case []any:
		return checkList(x)
	case map[any]any:
		return errors.New("mapping keys must be strings")
	}
	return nil
}
func checkList(list []any) error {
	for i, v := range list {
		if err := noInterpolation(v); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
	}
	return nil
}
func checkMap(m map[string]any) error {
	for key, val := range m {
		if interpolation(key) {
			return errors.New("compose interpolation is unsupported in keys")
		}
		if err := noInterpolation(val); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

var interpolationStart = regexp.MustCompile(`^[A-Za-z_{]`)

func interpolation(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			continue
		}
		if s[i+1] == '$' {
			i++
			continue
		}
		if interpolationStart.MatchString(s[i+1:]) {
			return true
		}
	}
	return false
}
func parseEndpoint(v any, services map[string]any) (Endpoint, error) {
	x, err := mapping(v, "x-launcher")
	if err != nil {
		return Endpoint{}, err
	}
	if err = fields(x, "x-launcher", "endpoint"); err != nil {
		return Endpoint{}, err
	}
	m, err := mapping(x["endpoint"], "x-launcher.endpoint")
	if err != nil {
		return Endpoint{}, err
	}
	if err = fields(m, "x-launcher.endpoint", "service", "port", "https"); err != nil {
		return Endpoint{}, err
	}
	endpoint, err := endpointValues(m)
	if err != nil {
		return Endpoint{}, err
	}
	if _, ok := services[endpoint.Service]; !ok {
		return Endpoint{}, errors.New("endpoint service does not exist")
	}
	return endpoint, nil
}
func endpointValues(m map[string]any) (Endpoint, error) {
	service, ok := m["service"].(string)
	if !ok || service == "" {
		return Endpoint{}, errors.New("endpoint service must be a nonempty string")
	}
	port, ok := m["port"].(int)
	if !ok || port < 1 || port > 65535 {
		return Endpoint{}, errors.New("endpoint port must be an integer between 1 and 65535")
	}
	https, err := endpointHTTPS(m)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Service: service, Port: port, HTTPS: https}, nil
}

func endpointHTTPS(m map[string]any) (bool, error) {
	v, exists := m["https"]
	if !exists {
		return true, nil
	}
	value, ok := v.(bool)
	if !ok {
		return false, errors.New("endpoint https must be a boolean")
	}
	return value, nil
}
func stringValue(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && strings.TrimSpace(s) != ""
}

// ProjectName returns the stable Compose identity for a stack.
func ProjectName(stackID string) string {
	sum := sha256.Sum256([]byte(stackID))
	return "launcher-" + hex.EncodeToString(sum[:])
}
