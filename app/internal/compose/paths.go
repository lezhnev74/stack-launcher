package compose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"stack-launcher.local/app/internal/ledger"
)

type pathContext struct {
	stateDir string
	manifest ledger.Manifest
}

var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func (c pathContext) resolve(path string, file bool) (string, error) {
	if !validRepositoryPath(path) {
		return "", errors.New("repository path must be relative and literal")
	}
	root, err := c.checkoutRoot()
	if err != nil {
		return "", err
	}
	resolved, err := resolvePath(root, filepath.Dir(c.manifest.Path), path)
	if err != nil {
		return "", err
	}
	if err = contained(root, resolved); err != nil {
		return "", err
	}
	if err = pathType(resolved, file); err != nil {
		return "", err
	}
	return strings.ReplaceAll(resolved, "$", "$$"), nil
}
func validRepositoryPath(path string) bool {
	return !filepath.IsAbs(path) && path != "" && !strings.Contains(path, "$")
}
func resolvePath(root, dir, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, dir, path))
	if err != nil {
		return "", fmt.Errorf("repository path: %w", err)
	}
	return filepath.Abs(resolved)
}
func (c pathContext) checkoutRoot() (string, error) {
	checkout := c.manifest.Checkout
	if !validCheckout(checkout, c.manifest.Path) {
		return "", errors.New("repository path requires a pinned Git checkout")
	}
	root := filepath.Join(c.stateDir, "checkouts", checkout)
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("checkout: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("checkout must be a directory")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("checkout: %w", err)
	}
	return filepath.Abs(root)
}
func validCheckout(checkout, path string) bool {
	return checkout != "" && checkout != "." && checkout != ".." && filepath.Base(checkout) == checkout && !filepath.IsAbs(path) && path != ""
}
func contained(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("repository path escapes checkout")
	}
	return nil
}
func pathType(path string, file bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return checkPathType(info, file)

}

func checkPathType(info os.FileInfo, file bool) error {
	if file {
		if !info.Mode().IsRegular() {
			return errors.New("repository file must be regular")
		}
		return nil
	}
	return checkBindType(info)
}
func checkBindType(info os.FileInfo) error {
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("repository bind must be a regular file or directory")
	}
	return nil
}

func prepareResources(root map[string]any, ctx pathContext) error {
	for _, key := range []string{"volumes", "networks", "configs", "secrets"} {
		v, exists := root[key]
		if !exists {
			continue
		}
		entries, err := mapping(v, key)
		if err != nil {
			return err
		}
		for name, value := range entries {
			if err := prepareResource(key, name, value, ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
func prepareResource(kind, name string, v any, ctx pathContext) error {
	label := kind + " " + name
	if !volumeName.MatchString(name) {
		return fmt.Errorf("%s: invalid name", label)
	}
	if kind == "volumes" || kind == "networks" {
		if v == nil {
			return nil
		}
		m, err := mapping(v, label)
		if err != nil {
			return err
		}
		return fields(m, label)
	}
	return prepareFileResource(label, v, ctx)
}
func prepareFileResource(label string, v any, ctx pathContext) error {
	m, err := mapping(v, label)
	if err != nil {
		return err
	}
	if err = fields(m, label, "file"); err != nil {
		return err
	}
	file, ok := stringValue(m["file"])
	if !ok {
		return fmt.Errorf("%s requires a file", label)
	}
	path, err := ctx.resolve(file, true)
	if err != nil {
		return fmt.Errorf("%s file: %w", label, err)
	}
	m["file"] = path
	return nil
}

func prepareMounts(s map[string]any, ctx pathContext) error {
	v, ok := s["volumes"]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return errors.New("volumes must be a list")
	}
	for i, item := range list {
		mount, err := prepareMount(item, ctx)
		if err != nil {
			return fmt.Errorf("volumes: %w", err)
		}
		list[i] = mount
	}
	return nil
}
func prepareMount(v any, ctx pathContext) (any, error) {
	switch mount := v.(type) {
	case string:
		return shortMount(mount, ctx)
	case map[string]any:
		return longMount(mount, ctx)
	default:
		return nil, errors.New("invalid mount")
	}
}
func shortMount(s string, ctx pathContext) (any, error) {
	parts := strings.Split(s, ":")
	if !validShortMount(parts) {
		return nil, errors.New("mount requires a source and absolute container target")
	}
	if strings.HasPrefix(parts[0], ".") || filepath.IsAbs(parts[0]) {
		return shortBind(parts, ctx)
	}
	return shortVolume(parts, s)

}
func shortVolume(parts []string, s string) (any, error) {
	if !volumeName.MatchString(parts[0]) {
		return nil, errors.New("volume source must be a valid name")
	}
	if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
		return nil, errors.New("unsupported volume mode")
	}
	return s, nil
}
func validShortMount(parts []string) bool {
	return len(parts) >= 2 && len(parts) <= 3 && parts[0] != "" && strings.HasPrefix(parts[1], "/")
}

func shortBind(parts []string, ctx pathContext) (any, error) {
	if len(parts) != 3 || parts[2] != "ro" {
		return nil, errors.New("repository bind mount must be read-only (:ro)")
	}
	path, err := ctx.resolve(parts[0], false)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "bind", "source": path, "target": parts[1], "read_only": true}, nil
}
func longMount(m map[string]any, ctx pathContext) (any, error) {
	if err := fields(m, "mount", "type", "source", "target", "read_only"); err != nil {
		return nil, err
	}
	source, err := mountValues(m)
	if err != nil {
		return nil, err
	}
	switch m["type"] {
	case "bind":
		return longBind(m, source, ctx)
	case "volume":
		if !volumeName.MatchString(source) {
			return nil, errors.New("volume source must be a name")
		}
	default:
		return nil, errors.New("mount type must be bind or volume")
	}
	return m, nil
}
func mountValues(m map[string]any) (string, error) {
	source, ok := stringValue(m["source"])
	if !ok {
		return "", errors.New("mount source is required; anonymous volumes are unsupported")
	}
	target, ok := stringValue(m["target"])
	if !ok || !strings.HasPrefix(target, "/") {
		return "", errors.New("mount target must be absolute")
	}
	if v, ok := m["read_only"]; ok {
		if _, ok := v.(bool); !ok {
			return "", errors.New("mount read_only must be boolean")
		}
	}
	return source, nil
}
func longBind(m map[string]any, source string, ctx pathContext) (any, error) {
	if m["read_only"] != true {
		return nil, errors.New("repository bind mount must set read_only: true")
	}
	path, err := ctx.resolve(source, false)
	if err != nil {
		return nil, err
	}
	m["source"] = path
	return m, nil
}
