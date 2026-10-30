package main

import (
	"strings"
	"testing"
)

// The comment is trimmed, kept otherwise unchanged, and limited in length.
func TestForwarderComment(t *testing.T) {
	forwarder := ForwarderConfig{Name: "x", Comment: "  mast cable\nask Jan  ",
		Input:   EndpointConfig{Mode: "udp-listen", Port: 29100},
		Outputs: []EndpointConfig{{Mode: "udp-send", Host: "127.0.0.1", Port: 29101}}}
	if err := forwarder.normalize(); err != nil {
		t.Fatal(err)
	}
	if forwarder.Comment != "mast cable\nask Jan" {
		t.Fatalf("comment = %q", forwarder.Comment)
	}
	forwarder.Comment = strings.Repeat("x", maxCommentLength+1)
	if err := forwarder.normalize(); err == nil {
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
