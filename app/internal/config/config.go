package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"stack-launcher.local/app/internal/caddy"
)

const defaultListen = "127.0.0.1:8080"

// DefaultComposeProvider is the executable used when ComposeProvider is omitted.
const DefaultComposeProvider = "podman-compose"

type Config struct {
	Listen           string
	StateDir         string
	ComposeProvider  string
	RegistryAuthFile string
	BaseDomain       string
	CaddyAdminSocket string
}

func Load(path string) (Config, error) {
	f, err := os.Open(path) // #nosec G304 -- operator chooses the configuration file
	if err != nil {
		return Config{}, fmt.Errorf("config %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	decoder := yaml.NewDecoder(f)
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("config %q: %w", path, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("config %q: expected exactly one YAML document", path)
	}
	fields, err := configFields(document.Content[0])
	if err != nil {
		return Config{}, fmt.Errorf("config %q: %w", path, err)
	}
	return buildConfig(path, fields)
}

func configFields(mapping *yaml.Node) (map[string]*yaml.Node, error) {
	if mapping.Kind != yaml.MappingNode {
		return nil, errors.New("expected a YAML mapping")
	}
	fields := make(map[string]*yaml.Node)
	for i := 0; i < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if key.Tag != "!!str" || !knownKey(key.Value) {
			return nil, fmt.Errorf("unknown key %q on line %d", key.Value, key.Line)
		}
		if _, exists := fields[key.Value]; exists {
			return nil, fmt.Errorf("duplicate key %q on line %d", key.Value, key.Line)
		}
		fields[key.Value] = mapping.Content[i+1]
	}
	return fields, nil
}

func knownKey(key string) bool {
	switch key {
	case "listen", "state_dir", "compose_provider", "registry_auth_file", "base_domain", "caddy_admin_socket":
		return true
	}
	return false
}

func buildConfig(path string, fields map[string]*yaml.Node) (Config, error) {
	listen, err := listenValue(path, fields["listen"])
	if err != nil {
		return Config{}, err
	}
	stateDir := fields["state_dir"]
	if !validString(stateDir) {
		return Config{}, fmt.Errorf("config %q: state_dir must be a non-empty string", path)
	}
	dir := stateDir.Value
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(path), dir)
	}
	if err := validateDir(dir); err != nil {
		return Config{}, fmt.Errorf("config %q: state_dir %q: %w", path, stateDir.Value, err)
	}
	provider, err := optionalPath(path, fields["compose_provider"], "compose_provider", DefaultComposeProvider, true)
	if err != nil {
		return Config{}, err
	}
	auth, err := optionalPath(path, fields["registry_auth_file"], "registry_auth_file", "", false)
	if err != nil {
		return Config{}, err
	}
	return finishConfig(path, fields, Config{Listen: listen, StateDir: dir, ComposeProvider: provider, RegistryAuthFile: auth})
}

func finishConfig(path string, fields map[string]*yaml.Node, cfg Config) (Config, error) {
	base, socket, err := caddyValues(path, fields)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseDomain, cfg.CaddyAdminSocket = base, socket
	return cfg, nil
}

func listenValue(path string, address *yaml.Node) (string, error) {
	if address == nil {
		return defaultListen, nil
	}
	if !validString(address) {
		return "", fmt.Errorf("config %q: listen must be a non-empty string", path)
	}
	return address.Value, nil
}

func validateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("must be a directory")
	}
	if info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0700 {
		return fmt.Errorf("must have mode 0700")
	}
	f, err := os.CreateTemp(dir, ".stack-launcher-check-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	closeErr := f.Close()
	removeErr := os.Remove(f.Name())
	if err := errors.Join(closeErr, removeErr); err != nil {
		return fmt.Errorf("temporary file cleanup: %w", err)
	}
	return nil
}

func validString(node *yaml.Node) bool {
	return node != nil && node.Tag == "!!str" && node.Value != ""
}

func optionalPath(configPath string, node *yaml.Node, key, fallback string, executable bool) (string, error) {
	if node == nil {
		return fallback, nil
	}
	if !validString(node) || strings.TrimSpace(node.Value) == "" {
		return "", fmt.Errorf("config %q: %s must be a non-empty string", configPath, key)
	}
	value := node.Value
	if pathNeedsResolution(value, executable) {
		var err error
		value, err = filepath.Abs(filepath.Join(filepath.Dir(configPath), value))
		if err != nil {
			return "", fmt.Errorf("config %q: %s: %w", configPath, key, err)
		}
	}
	return value, nil
}

func pathNeedsResolution(value string, executable bool) bool {
	return !filepath.IsAbs(value) && (!executable || strings.Contains(value, "/"))
}

func caddyValues(path string, fields map[string]*yaml.Node) (string, string, error) {
	base, socket := fields["base_domain"], fields["caddy_admin_socket"]
	if base == nil && socket == nil {
		return "", "", nil
	}
	if !validBase(base) {
		return "", "", fmt.Errorf("config %q: base_domain must be a valid DNS name", path)
	}
	if invalidSocket(socket) {
		return "", "", fmt.Errorf("config %q: caddy_admin_socket must be a Unix socket path", path)
	}
	value, err := optionalPath(path, socket, "caddy_admin_socket", "", false)
	if err != nil {
		return "", "", err
	}
	if value == "" {
		return "", "", fmt.Errorf("config %q: caddy_admin_socket must be a Unix socket path", path)
	}
	return strings.ToLower(base.Value), value, nil
}

func invalidSocket(socket *yaml.Node) bool {
	return socket != nil && strings.Contains(socket.Value, "://")
}

func validBase(node *yaml.Node) bool { return validString(node) && caddy.ValidDomain(node.Value) }
