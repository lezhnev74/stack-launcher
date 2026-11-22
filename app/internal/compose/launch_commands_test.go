package compose

import (
	"reflect"
	"strings"
	"testing"
)

func TestLaunchCommandsOrderAndLiterals(t *testing.T) {
	source := `services:
  web:
    image: web
    depends_on: [middle]
    x-launcher: {launch_command: [echo, '$$HOME', '']}
  middle:
    image: middle
    depends_on: {zdb: {condition: service_started}}
  zdb:
    image: db
    x-launcher: {launch_command: [seed]}
x-launcher: {endpoint: {service: web, port: 80}}
`
	p, err := prepare(t, source)
	if err != nil {
		t.Fatal(err)
	}
	want := []LaunchCommand{{"zdb", []string{"seed"}}, {"web", []string{"echo", "$HOME", ""}}}
	if !reflect.DeepEqual(p.LaunchCommands, want) || strings.Contains(string(p.YAML), "launch_command") {
		t.Fatalf("commands %#v; YAML %s", p.LaunchCommands, p.YAML)
	}
	scalar := strings.Replace(source, "depends_on: {zdb: {condition: service_started}}", "depends_on: zdb", 1)
	p, err = prepare(t, scalar)
	if err != nil || !reflect.DeepEqual(p.LaunchCommands, want) {
		t.Fatalf("scalar dependencies: %#v, %v", p.LaunchCommands, err)
	}
}

func TestInvalidLaunchCommandsAndDependencies(t *testing.T) {
	for _, spec := range []string{
		"x-launcher: {launch_command: []}",
		"x-launcher: {launch_command: ['', arg]}",
		"x-launcher: {launch_command: [echo, 1]}",
		"x-launcher: {launch_command: [echo, \"\\0\"]}",
		"x-launcher: {other: yes}",
		"x-launcher: null",
		"depends_on: [missing]\n    x-launcher: {launch_command: [echo]}",
		"depends_on: [web]\n    x-launcher: {launch_command: [echo]}",
		"depends_on: [1]\n    x-launcher: {launch_command: [echo]}",
		"depends_on: null\n    x-launcher: {launch_command: [echo]}",
		"depends_on: {web: null}\n    x-launcher: {launch_command: [echo]}",
	} {
		source := "services:\n  web:\n    image: web\n    " + spec + "\nx-launcher: {endpoint: {service: web, port: 80}}\n"
		if _, err := prepare(t, source); err == nil {
			t.Errorf("accepted %s", spec)
		}
	}
}
