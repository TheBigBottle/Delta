package manager

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"delta/internal/config"
)

// freeTestPort returns a TCP port that is free on the loopback interface.
func freeTestPort(t testing.TB) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Regression test: a forwarder created without tags must still have a "tags"
// list in its JSON, otherwise the web interface cannot render the list.
func TestCreatedForwarderHasTagList(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir + "/delta.json")
	if err != nil {
		t.Fatal(err)
	}
	m := New(dir+"/delta.json", cfg)
	status, err := m.Create(config.ForwarderConfig{
		Name:    "no tags",
		Input:   config.EndpointConfig{Mode: "udp-listen", Port: freeTestPort(t)},
		Outputs: []config.EndpointConfig{{Mode: "udp-send", Host: "127.0.0.1", Port: 9}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{status, m.List()[0]} {
		data, _ := json.Marshal(v)
		if !strings.Contains(string(data), `"tags":[]`) {
			t.Fatalf("tags missing in %s", data)
		}
	}
}

// "Start with Delta": Create keeps the flag (and starting switches it on),
// Update sets it only when asked to, and never starts or stops the forwarder.
func TestAutostartFromEditor(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir + "/delta.json")
	if err != nil {
		t.Fatal(err)
	}
	m := New(dir+"/delta.json", cfg)
	defer m.Shutdown()
	newConfig := func(autostart bool) config.ForwarderConfig {
		return config.ForwarderConfig{
			Name:      "autostart",
			Autostart: autostart,
			Input:     config.EndpointConfig{Mode: "udp-listen", Port: freeTestPort(t)},
			Outputs:   []config.EndpointConfig{{Mode: "udp-send", Host: "127.0.0.1", Port: 9}},
		}
	}
	autostartOf := func(id string) bool {
		status, _ := m.Get(id)
		return status.Autostart
	}

	ticked, err := m.Create(newConfig(true), false)
	if err != nil {
		t.Fatal(err)
	}
	if !autostartOf(ticked.ID) || ticked.State == "running" {
		t.Fatalf("created with the box ticked: autostart %v, state %s (want true, not running)", autostartOf(ticked.ID), ticked.State)
	}
	started, err := m.Create(newConfig(false), true)
	if err != nil {
		t.Fatal(err)
	}
	if !autostartOf(started.ID) {
		t.Fatal("Create & start did not switch autostart on")
	}

	// nil (key missing in the request) keeps the value.
	if _, err := m.Update(ticked.ID, newConfig(false), nil); err != nil {
		t.Fatal(err)
	}
	if !autostartOf(ticked.ID) {
		t.Fatal("an update without autostart switched it off")
	}
	off := false
	if _, err := m.Update(started.ID, newConfig(true), &off); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Get(started.ID)
	if status.Autostart || status.State != "running" {
		t.Fatalf("after unticking: autostart %v, state %s (want false, still running)", status.Autostart, status.State)
	}

	// Start and Stop still change the flag.
	if err := m.SetRunning("start", ticked.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRunning("stop", ticked.ID); err != nil {
		t.Fatal(err)
	}
	if autostartOf(ticked.ID) {
		t.Fatal("Stop did not switch autostart off")
	}
}
