package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// The comment is trimmed, kept otherwise unchanged, and limited in length.
func TestForwarderComment(t *testing.T) {
	forwarder := ForwarderConfig{Name: "x", Comment: "  mast cable\nask Jan  ",
		Input:   EndpointConfig{Mode: "udp-listen", Port: 29100},
		Outputs: []EndpointConfig{{Mode: "udp-send", Host: "127.0.0.1", Port: 29101}}}
	if err := forwarder.Normalize(); err != nil {
		t.Fatal(err)
	}
	if forwarder.Comment != "mast cable\nask Jan" {
		t.Fatalf("comment = %q", forwarder.Comment)
	}
	forwarder.Comment = strings.Repeat("x", maxCommentLength+1)
	if err := forwarder.Normalize(); err == nil {
		t.Fatal("an over-long comment was accepted")
	}
}

// Additional hosts are only kept for tcp-connect, and an address may not appear twice.
func TestConnectTargetsValidation(t *testing.T) {
	endpoint := EndpointConfig{Mode: "tcp-connect", Host: "10.0.0.5", Port: 4001,
		Targets: []RemoteTarget{{Host: " 10.0.0.6 ", Port: 4001}}}
	if err := endpoint.validate(true); err != nil || endpoint.Targets[0].Host != "10.0.0.6" {
		t.Fatalf("valid targets rejected or not trimmed: %v %+v", err, endpoint.Targets)
	}
	endpoint.Targets = append(endpoint.Targets, RemoteTarget{Host: "10.0.0.5", Port: 4001})
	if err := endpoint.validate(true); err == nil {
		t.Fatal("a duplicate address was accepted")
	}
	listener := EndpointConfig{Mode: "tcp-listen", Port: 4001, Targets: []RemoteTarget{{Host: "x", Port: 1}}}
	if err := listener.validate(true); err != nil || listener.Targets != nil {
		t.Fatalf("targets of a listen endpoint were not cleared: %v %+v", err, listener.Targets)
	}
}

// The removed udp-connect mode gives a clear error, also for old files that
// still contain its hello settings.
func TestRemovedUDPConnectMode(t *testing.T) {
	path := t.TempDir() + "/delta.json"
	old := `{"forwarders":[{"name":"old","framing":"lines","input":{"mode":"udp-connect","host":"ais.example.org","port":5631,"hello":"HELLO","hello_interval":30},"outputs":[{"mode":"tcp-listen","port":10112}]}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("error = %v, want the removed-mode message", err)
	}
}

// Extra hosts of a tcp-connect output (possible until 1.9.0) become separate
// outputs with the same label and queue; an input keeps its extra hosts.
func TestOutputTargetsBecomeSeparateOutputs(t *testing.T) {
	forwarder := ForwarderConfig{
		Name:  "split",
		Input: EndpointConfig{Mode: "tcp-connect", Host: "10.0.0.1", Port: 4001, Targets: []RemoteTarget{{Host: "10.0.0.2", Port: 4001}}},
		Outputs: []EndpointConfig{
			{Mode: "tcp-connect", Name: "plotter", Host: "10.0.0.5", Port: 10110, QueueSize: 50,
				Targets: []RemoteTarget{{Host: " 10.0.0.6 ", Port: 10111}, {Host: "10.0.0.7", Port: 10112}}},
			{Mode: "udp-send", Host: "10.0.0.8", Port: 10113},
		},
	}
	if err := forwarder.Normalize(); err != nil {
		t.Fatal(err)
	}
	want := []EndpointConfig{
		{Mode: "tcp-connect", Name: "plotter", Host: "10.0.0.5", Port: 10110, QueueSize: 50},
		{Mode: "tcp-connect", Name: "plotter", Host: "10.0.0.6", Port: 10111, QueueSize: 50},
		{Mode: "tcp-connect", Name: "plotter", Host: "10.0.0.7", Port: 10112, QueueSize: 50},
		{Mode: "udp-send", Host: "10.0.0.8", Port: 10113},
	}
	if !reflect.DeepEqual(forwarder.Outputs, want) {
		t.Fatalf("outputs:\n got %+v\nwant %+v", forwarder.Outputs, want)
	}
	if len(forwarder.Input.Targets) != 1 {
		t.Fatalf("input targets were changed: %+v", forwarder.Input.Targets)
	}
}
