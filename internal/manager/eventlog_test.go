package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"delta/internal/config"
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
		"":             "/etc/delta/" + config.DefaultEventLog,
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

// A line written to the file reads back as the same event, also for
// messages with colons and line breaks.
func TestEventLogLineFormat(t *testing.T) {
	when := time.Date(2026, 10, 30, 20, 37, 41, 112_000_000, time.Local)
	events := []Event{
		{Time: when, Level: "info", Message: "Delta 1.3.0 started"},
		{Time: when, Level: "error", ForwarderID: "f3a9c2e17b04", ForwarderName: "GPS bridge", Message: "connect 10.0.0.5:4001: connection refused"},
		{Time: when, Level: "warn", ForwarderID: "f000000000aa", ForwarderName: "AIS [deck]", Message: "two\nlines"},
	}
	for _, want := range events {
		line := formatLogLine(want)
		if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
			t.Fatalf("not exactly one line: %q", line)
		}
		got, ok := parseLogLine(strings.TrimSuffix(line, "\n"))
		want.Message = strings.ReplaceAll(want.Message, "\n", " ")
		if !ok || !got.Time.Equal(want.Time) || got.Level != want.Level || got.ForwarderID != want.ForwarderID ||
			got.ForwarderName != want.ForwarderName || got.Message != want.Message {
			t.Errorf("line %q read back as %+v, want %+v", line, got, want)
		}
	}
	if _, ok := parseLogLine(`{"id":1,"msg":"json from an older build"}`); ok {
		t.Error("a line in another format was accepted")
	}
}
