package podman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
)

const readinessTimeout = 30 * time.Second

var errInvalidContainer = errors.New("invalid container")

// readinessWait carries a container observation safe to show in launch activity.
type readinessWait string

func (e readinessWait) Error() string { return string(e) }

type container struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
		Health   *struct {
			Status string `json:"Status"`
		} `json:"Health"`
		Healthcheck *struct {
			Status string `json:"Status"`
		} `json:"Healthcheck"`
	} `json:"State"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func waitReady(ctx context.Context, r runner, prepared compose.Prepared, images map[string]ledger.Image) (Result, error) {
	started := time.Now()
	r.logf("Checking container readiness services=%d timeout=%s", len(prepared.Services), readinessTimeout)
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	var lastReason string
	for {
		result, err := inspectProject(ctx, r, prepared, images)
		if err == nil {
			reportProgress(ctx, "All containers and port bindings are ready.")
			r.logf("Containers ready elapsed=%s", time.Since(started))
			return result, nil
		}
		if errors.Is(err, errInvalidContainer) {
			return Result{}, fmt.Errorf("project %q readiness: %w", prepared.ProjectName, err)
		}
		if reason := err.Error(); reason != lastReason {
			r.logf("Waiting for container readiness elapsed=%s reason=%q", time.Since(started), reason)
			reportReadinessWait(ctx, err)
			lastReason = reason
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Result{}, fmt.Errorf("project %q readiness: %w: %w", prepared.ProjectName, ctx.Err(), err)
		case <-timer.C:
		}
	}
}

func reportReadinessWait(ctx context.Context, err error) {
	var waiting readinessWait
	if errors.As(err, &waiting) {
		reportProgress(ctx, "Waiting for readiness: %s.", waiting)
	}
}

func projectContainers(ctx context.Context, r runner, project string) ([]string, error) {
	out, err := r.run(ctx, "ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}
func inspectProject(ctx context.Context, r runner, prepared compose.Prepared, images map[string]ledger.Image) (Result, error) {
	ids, err := projectContainers(ctx, r, prepared.ProjectName)
	if err != nil {
		return Result{}, err
	}
	if len(ids) != len(prepared.Services) {
		return Result{}, readinessWait(fmt.Sprintf("expected %d containers, found %d", len(prepared.Services), len(ids)))
	}
	out, err := r.run(ctx, append([]string{"container", "inspect"}, ids...)...)
	if err != nil {
		return Result{}, err
	}
	var containers []container
	if err = json.Unmarshal([]byte(out), &containers); err != nil {
		return Result{}, fmt.Errorf("decode container inspection: %w", err)
	}
	return verifyContainers(containers, prepared, images)
}
func verifyContainers(containers []container, prepared compose.Prepared, images map[string]ledger.Image) (Result, error) {
	bindings := make(map[string]map[int]netip.AddrPort)
	ids := map[string]string{}
	for _, c := range containers {
		name, ports, err := inspectService(c, prepared, images, ids)
		if err != nil {
			return Result{}, err
		}
		bindings[name] = ports
		if len(prepared.LaunchCommands) > 0 && !fullID.MatchString(c.ID) {
			return Result{}, fmt.Errorf("%w: service %q: invalid container ID", errInvalidContainer, name)
		}
		ids[name] = c.ID
	}
	if len(ids) != len(prepared.Services) {
		return Result{}, errors.New("container inspection omitted a service")
	}
	endpoint, ok := bindings[prepared.Endpoint.Service][prepared.Endpoint.Port]
	if !ok {
		return Result{}, errors.New("endpoint binding is missing")
	}
	return Result{ProjectName: prepared.ProjectName, Endpoint: endpoint, HTTPS: prepared.Endpoint.HTTPS, Bindings: bindings, containerIDs: ids}, nil
}
func inspectService(c container, prepared compose.Prepared, images map[string]ledger.Image, ids map[string]string) (string, map[int]netip.AddrPort, error) {
	name := c.Config.Labels["com.docker.compose.service"]
	service, ok := prepared.Services[name]
	_, seen := ids[name]
	if !ok || seen || c.Config.Labels["com.docker.compose.project"] != prepared.ProjectName {
		return "", nil, fmt.Errorf("%w: unexpected or duplicate project container for service %q", errInvalidContainer, name)
	}
	if err := verifyContainer(c, name, images[name]); err != nil {
		return "", nil, err
	}
	ports, err := verifyPorts(c, name, service.Ports)
	return name, ports, err
}
func verifyContainer(c container, name string, image ledger.Image) error {
	if strings.TrimPrefix(c.Image, "sha256:") != image.ID {
		return fmt.Errorf("%w: service %q: container image does not match pinned image", errInvalidContainer, name)
	}

	if c.State.Status == "exited" {
		return fmt.Errorf("%w: service %q: container exited with code %d", errInvalidContainer, name, c.State.ExitCode)
	}
	if c.State.Status != "running" {
		return readinessWait(fmt.Sprintf("service %q: container is %s", name, c.State.Status))
	}
	return verifyHealth(c, name)
}
func verifyHealth(c container, name string) error {
	if c.State.Health != nil && c.State.Health.Status != "healthy" {
		return readinessWait(fmt.Sprintf("service %q: health is %s", name, c.State.Health.Status))
	}
	if c.State.Health == nil && c.State.Healthcheck != nil && c.State.Healthcheck.Status != "healthy" {
		return readinessWait(fmt.Sprintf("service %q: health is %s", name, c.State.Healthcheck.Status))
	}
	return nil
}
func verifyPorts(c container, name string, expected []int) (map[int]netip.AddrPort, error) {
	ports := map[int]netip.AddrPort{}
	for key, entries := range c.NetworkSettings.Ports {
		if len(entries) == 0 {
			continue
		}
		port, binding, err := parseBinding(name, key, entries, expected)
		if err != nil {
			return nil, err
		}
		ports[port] = binding
	}
	if len(ports) != len(expected) {
		return nil, readinessWait(fmt.Sprintf("service %q: expected published ports are missing", name))
	}
	return ports, nil
}
func parseBinding(name, key string, entries []struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}, expected []int) (int, netip.AddrPort, error) {
	raw, proto, ok := strings.Cut(key, "/")
	port, err := strconv.Atoi(raw)
	if !ok || err != nil || !validPublishedPort(proto, port, expected, len(entries)) {
		return 0, netip.AddrPort{}, fmt.Errorf("%w: service %q: unexpected published port %q", errInvalidContainer, name, key)
	}
	hostPort, err := strconv.ParseUint(entries[0].HostPort, 10, 16)
	if err != nil || hostPort == 0 || entries[0].HostIP != "127.0.0.1" {
		return 0, netip.AddrPort{}, fmt.Errorf("%w: service %q: unsafe binding for port %d", errInvalidContainer, name, port)
	}
	return port, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(hostPort)), nil
}
func validPublishedPort(proto string, port int, expected []int, entries int) bool {
	return proto == "tcp" && containsPort(expected, port) && entries == 1
}
func containsPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}
