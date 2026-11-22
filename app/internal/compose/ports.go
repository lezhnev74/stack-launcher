package compose

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

func preparePorts(s map[string]any, endpoint int) error {
	var entries []any
	if v, ok := s["ports"]; ok {
		var valid bool
		entries, valid = v.([]any)
		if !valid {
			return errors.New("ports must be a list")
		}
	}
	out, seen, err := normalizePorts(entries)
	if err != nil {
		return err
	}
	if endpoint > 0 && !seen[endpoint] {
		out = append(out, portBinding(endpoint))
	}
	if len(out) > 0 {
		s["ports"] = out
	}
	return nil
}
func normalizePorts(entries []any) ([]any, map[int]bool, error) {
	seen := map[int]bool{}
	out := []any{}
	for _, entry := range entries {
		port, err := parsePort(entry)
		if err != nil {
			return nil, nil, fmt.Errorf("ports: %w", err)
		}
		if !seen[port] {
			out = append(out, portBinding(port))
			seen[port] = true
		}
	}
	return out, seen, nil
}

func portBinding(port int) map[string]any {
	return map[string]any{"target": port, "host_ip": "127.0.0.1", "protocol": "tcp"}
}

func containerPorts(service map[string]any) []int {
	entries, _ := service["ports"].([]any)
	var ports []int
	for _, entry := range entries {
		ports = append(ports, entry.(map[string]any)["target"].(int))
	}
	return ports
}

func parsePort(v any) (int, error) {
	switch p := v.(type) {
	case int:
		return numberPort(p)
	case string:
		return shortPort(p)
	case map[string]any:
		return longPort(p)
	default:
		return 0, errors.New("invalid port entry")
	}
}
func numberPort(n int) (int, error) {
	if n < 1 || n > 65535 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	return n, nil
}

var decimalPort = regexp.MustCompile(`^[0-9]+$`)

func textPort(s string) (int, error) {
	if !decimalPort.MatchString(s) {
		return 0, errors.New("port must be a single number")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("port must be a single number")
	}
	return numberPort(n)
}
func shortPort(s string) (int, error) {
	if strings.Contains(s, "/") {
		var proto string
		s, proto, _ = strings.Cut(s, "/")
		if proto != "tcp" {
			return 0, errors.New("only TCP ports are supported")
		}
	}
	if strings.HasPrefix(s, "[") {
		return bracketPort(s)
	}
	return shortPortParts(strings.Split(s, ":"))
}
func bracketPort(s string) (int, error) {
	host, rest, ok := strings.Cut(s, "]:")
	if !ok || !isLoopback(strings.TrimPrefix(host, "[")) {
		return 0, errors.New("port host IP must be loopback")
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 2 {
		return 0, errors.New("invalid short port")
	}
	return shortPortParts(parts)
}
func isLoopback(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.IsLoopback()
}
func isLoopbackValue(value any) bool { ip, ok := value.(string); return ok && isLoopback(ip) }
func shortPortParts(parts []string) (int, error) {
	if len(parts) > 3 {
		return 0, errors.New("invalid short port")
	}
	if len(parts) == 3 && !isLoopback(parts[0]) {
		return 0, errors.New("port host IP must be loopback")
	}
	if len(parts) > 1 && parts[len(parts)-2] != "" {
		if _, err := textPort(parts[len(parts)-2]); err != nil {
			return 0, err
		}
	}
	return textPort(parts[len(parts)-1])
}
func longPort(p map[string]any) (int, error) {
	if err := fields(p, "port", "target", "published", "host_ip", "protocol"); err != nil {
		return 0, err
	}
	if err := longPortOptions(p); err != nil {
		return 0, err
	}
	return portValue(p["target"])
}
func longPortOptions(p map[string]any) error {
	if ip, ok := p["host_ip"]; ok && !isLoopbackValue(ip) {
		return errors.New("port host IP must be loopback")
	}
	if proto, ok := p["protocol"]; ok && proto != "tcp" {
		return errors.New("only TCP ports are supported")
	}
	if published, ok := p["published"]; ok {
		if _, err := portValue(published); err != nil {
			return err
		}
	}
	return nil
}

func portValue(v any) (int, error) {
	switch p := v.(type) {
	case int:
		return numberPort(p)
	case string:
		return textPort(p)
	default:
		return 0, errors.New("port must be a single number")
	}
}
