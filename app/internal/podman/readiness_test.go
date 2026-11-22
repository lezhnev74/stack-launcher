package podman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
)

func readyFixture() (container, compose.Prepared, map[string]ledger.Image) {
	c := container{Image: testImage}
	c.Config.Labels = map[string]string{"com.docker.compose.project": "p", "com.docker.compose.service": "web"}
	c.State.Status = "running"
	c.NetworkSettings.Ports = map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "1234"}}}
	return c, compose.Prepared{ProjectName: "p", Endpoint: compose.Endpoint{Service: "web", Port: 80}, Services: map[string]compose.Service{"web": {Ports: []int{80}}}}, map[string]ledger.Image{"web": {ID: testImage}}
}
func TestVerifyContainers(t *testing.T) {
	c, p, images := readyFixture()
	if _, err := verifyContainers([]container{c}, p, images); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		mutate  func(*container, *compose.Prepared)
		invalid bool
		message string
	}{
		{"wrong image", func(c *container, _ *compose.Prepared) { c.Image = "bad" }, true, "image"},
		{"wrong project", func(c *container, _ *compose.Prepared) { c.Config.Labels["com.docker.compose.project"] = "other" }, true, "unexpected"},
		{"unknown service", func(c *container, _ *compose.Prepared) { c.Config.Labels["com.docker.compose.service"] = "bad" }, true, "unexpected"},
		{"invalid launch command target", func(_ *container, p *compose.Prepared) {
			p.LaunchCommands = []compose.LaunchCommand{{Service: "web", Args: []string{"seed"}}}
		}, true, "invalid container ID"},
		{"exited", func(c *container, _ *compose.Prepared) { c.State.Status = "exited"; c.State.ExitCode = 3 }, true, "exited"},
		{"starting", func(c *container, _ *compose.Prepared) { c.State.Status = "created" }, false, "created"},
		{"health starting", func(c *container, _ *compose.Prepared) {
			c.State.Health = &struct {
				Status string `json:"Status"`
			}{"starting"}
		}, false, "starting"},
		{"healthcheck unhealthy", func(c *container, _ *compose.Prepared) {
			c.State.Healthcheck = &struct {
				Status string `json:"Status"`
			}{"unhealthy"}
		}, false, "unhealthy"},
		{"unsafe address", func(c *container, _ *compose.Prepared) { c.NetworkSettings.Ports["80/tcp"][0].HostIP = "0.0.0.0" }, true, "unsafe"},
		{"zero port", func(c *container, _ *compose.Prepared) { c.NetworkSettings.Ports["80/tcp"][0].HostPort = "0" }, true, "unsafe"},
		{"unexpected protocol", func(c *container, _ *compose.Prepared) {
			c.NetworkSettings.Ports["80/udp"] = c.NetworkSettings.Ports["80/tcp"]
			delete(c.NetworkSettings.Ports, "80/tcp")
		}, true, "unexpected"},
		{"missing port", func(c *container, _ *compose.Prepared) { delete(c.NetworkSettings.Ports, "80/tcp") }, false, "missing"},
		{"endpoint missing", func(_ *container, p *compose.Prepared) { p.Endpoint.Port = 81 }, false, "endpoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, p, images := readyFixture()
			tt.mutate(&c, &p)
			_, err := verifyContainers([]container{c}, p, images)
			if err == nil || !strings.Contains(err.Error(), tt.message) || errors.Is(err, errInvalidContainer) != tt.invalid {
				t.Fatalf("%v", err)
			}
		})
	}
	if _, err := verifyContainers([]container{c, c}, p, images); !errors.Is(err, errInvalidContainer) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := verifyContainers(nil, p, images); err == nil {
		t.Fatal("accepted missing service")
	}
}
func TestWaitReadyFailures(t *testing.T) {
	cfg, _, dir := fakeRuntime(t)
	r, err := newRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, p, images := readyFixture()
	p.ProjectName = "wrong"
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err = waitReady(ctx, r, p, images); err == nil {
		t.Fatal("expected readiness error")
	}
	_ = dir
}

func TestHealthTransitionsToReady(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	if err := os.WriteFile(filepath.Join(dir, "starting"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Launch(t.Context(), cfg, db, "one", nil)
	if err != nil || result.Endpoint.Port() != 12345 {
		t.Fatalf("readiness: %+v, %v", result, err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test-owned temporary fixture
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "container inspect") < 2 {
		t.Fatalf("health was not rechecked: %s", log)
	}
}
