package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Events survive a restart: they are appended to the log file and loaded
// again by the next EventLog that opens it.
func TestEventLogFileSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	bus := &EventBus{subscribers: map[chan []byte]struct{}{}}
	first := newEventLog(bus)
	if err := first.OpenFile(path, 1<<20); err != nil {
		t.Fatal(err)
	}
	first.Add("info", "f1", "GPS", "started")
	first.Add("error", "f1", "GPS", "connection refused")
	first.CloseFile()

	second := newEventLog(bus)
	if err := second.OpenFile(path, 1<<20); err != nil {
		t.Fatal(err)
	}
	second.Add("info", "", "", "Delta started")
	events := second.NewestFirst("", 10)
	if len(events) != 3 || events[0].Message != "Delta started" || events[2].Message != "started" || events[1].Level != "error" {
		t.Fatalf("events after restart = %+v", events)
	}
	if events[0].ID != 3 {
		t.Fatalf("numbering does not continue after the loaded events: id %d", events[0].ID)
	}
	if only := second.NewestFirst("f1", 10); len(only) != 2 {
		t.Fatalf("forwarder filter returned %d events, want 2", len(only))
	}
}

// A full file is rotated to "<file>.1", and both parts are loaded at startup.
func TestEventLogRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	bus := &EventBus{subscribers: map[chan []byte]struct{}{}}
	log := newEventLog(bus)
	if err := log.OpenFile(path, 1000); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		log.Add("info", "", "", "a message that is long enough to fill the file quickly")
	}
	log.CloseFile()
	for _, name := range []string{path, path + ".1"} {
		info, err := os.Stat(name)
		if err != nil || info.Size() > 1000 {
			t.Fatalf("%s: %v, size limit not kept", name, err)
		}
	}
	reloaded := newEventLog(bus)
	if err := reloaded.OpenFile(path, 1000); err != nil {
		t.Fatal(err)
	}
	if count := len(reloaded.NewestFirst("", 100)); count < 5 || count >= 30 {
		t.Fatalf("loaded %d events from both parts", count)
	}
}

// "off" switches the file off; relative paths are next to the config file.
func TestResolveEventLogPath(t *testing.T) {
	cases := map[string]string{
		"":             "/etc/delta/" + defaultEventLog,
		"OFF":          "",
		"logs/ev.log":  "/etc/delta/logs/ev.log",
		"/var/log/d.l": "/var/log/d.l",
	}
	for configured, want := range cases {
		if got := resolveEventLogPath("/etc/delta/delta.json", configured); got != want {
			t.Errorf("resolveEventLogPath(%q) = %q, want %q", configured, got, want)
		}
	}
}
