package compose

import (
	"fmt"
	"sort"
	"strings"
)

// LaunchCommand is a fresh-launch command and its target service.
type LaunchCommand struct {
	Service string
	Args    []string
}

func extractLaunchCommands(services map[string]any) ([]LaunchCommand, error) {
	commands, err := collectLaunchCommands(services)
	if err != nil || len(commands) == 0 {
		return nil, err
	}
	return orderLaunchCommands(services, commands)
}

func collectLaunchCommands(services map[string]any) (map[string][]string, error) {
	commands := map[string][]string{}
	for name, value := range services {
		service := value.(map[string]any)
		args, exists, err := serviceLaunchCommand(service)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
		if exists {
			commands[name] = args
		}
	}
	return commands, nil
}

func serviceLaunchCommand(service map[string]any) ([]string, bool, error) {
	value, exists := service["x-launcher"]
	if !exists {
		return nil, false, nil
	}
	extension, err := mapping(value, "x-launcher")
	if err != nil {
		return nil, false, err
	}
	if err = fields(extension, "x-launcher", "launch_command", "git"); err != nil {
		return nil, false, err
	}
	delete(service, "x-launcher")
	value, exists = extension["launch_command"]
	if !exists {
		if _, ok := extension["git"]; !ok {
			return nil, false, fmt.Errorf("x-launcher requires launch_command or git")
		}
		return nil, false, nil
	}
	args, err := launchArgs(value)
	return args, true, err
}

func orderLaunchCommands(services map[string]any, commands map[string][]string) ([]LaunchCommand, error) {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	ordering := commandOrdering{services: services, commands: commands, state: map[string]int{}}
	for _, name := range names {
		if err := ordering.visit(name); err != nil {
			return nil, err
		}
	}
	return ordering.order, nil
}

type commandOrdering struct {
	services map[string]any
	commands map[string][]string
	state    map[string]int
	order    []LaunchCommand
}

const (
	visiting = iota + 1
	visited
)

func (o *commandOrdering) visit(name string) error {
	if o.state[name] == visiting {
		return fmt.Errorf("dependency cycle involving service %q", name)
	}
	if o.state[name] == visited {
		return nil
	}
	o.state[name] = visiting
	deps, err := dependencies(o.services[name].(map[string]any))
	if err != nil {
		return fmt.Errorf("service %q: %w", name, err)
	}
	sort.Strings(deps)
	if err := o.visitDependencies(name, deps); err != nil {
		return err
	}
	o.state[name] = visited
	if args, ok := o.commands[name]; ok {
		o.order = append(o.order, LaunchCommand{Service: name, Args: args})
	}
	return nil
}
func (o *commandOrdering) visitDependencies(name string, deps []string) error {
	for _, dep := range deps {
		if _, ok := o.services[dep]; !ok {
			return fmt.Errorf("service %q: unknown dependency %q", name, dep)
		}
		if err := o.visit(dep); err != nil {
			return err
		}
	}
	return nil
}

func launchArgs(value any) ([]string, error) {
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("launch_command must be a nonempty list of strings")
	}
	args := make([]string, len(list))
	for i, value := range list {
		arg, ok := value.(string)
		if !ok || strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("launch_command arguments must be strings without NUL")
		}
		args[i] = strings.ReplaceAll(arg, "$$", "$")
	}
	if strings.TrimSpace(args[0]) == "" {
		return nil, fmt.Errorf("launch_command executable must be nonblank")
	}
	return args, nil
}
func dependencies(service map[string]any) ([]string, error) {
	value, exists := service["depends_on"]
	if !exists {
		return nil, nil
	}
	switch deps := value.(type) {
	case string:
		return []string{deps}, nil
	case []any:
		return dependencyList(deps)
	case map[string]any:
		return dependencyMap(deps)
	default:
		return nil, fmt.Errorf("depends_on must be a service name, list or mapping")
	}
}

func dependencyMap(deps map[string]any) ([]string, error) {
	result := make([]string, 0, len(deps))
	for name, options := range deps {
		if _, ok := options.(map[string]any); !ok {
			return nil, fmt.Errorf("depends_on options for %q must be a mapping", name)
		}
		result = append(result, name)
	}
	return result, nil
}

func dependencyList(deps []any) ([]string, error) {
	result := make([]string, 0, len(deps))
	for _, value := range deps {
		name, ok := value.(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("depends_on entries must be service names")
		}
		result = append(result, name)
	}
	return result, nil
}
