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
	if err := first.OpenFile(path, 1); err != nil {
		t.Fatal(err)
	}
	first.Add("info", "f1", "GPS", "started")
	first.Add("error", "f1", "GPS", "connection refused")
	first.CloseFile()

	second := newEventLog(bus)
	if err := second.OpenFile(path, 1); err != nil {
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

// A full file is rotated (lumberjack: "<name>-<timestamp><ext>"), only one
// rotated part is kept, and the rotated part plus the current file are
// loaded at startup, together with a "<file>.1" left by older versions.
func TestEventLogRotation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "events.log")
	legacyLine := formatLogLine(Event{Time: time.Now(), Level: "info", Message: "from an older Delta"})
	if err := os.WriteFile(path+".1", []byte(legacyLine), 0o600); err != nil {
		t.Fatal(err)
	}
	bus := &EventBus{subscribers: map[chan []byte]struct{}{}}
	log := newEventLog(bus)
	if err := log.OpenFile(path, 1); err != nil {
		t.Fatal(err)
	}
	if loaded := log.NewestFirst("", 10); len(loaded) != 1 || loaded[0].Message != "from an older Delta" {
		t.Fatalf("legacy .1 file not loaded: %+v", loaded)
	}
	message := strings.Repeat("a message that is long enough to fill the file quickly ", 4)
	for range 2 * (1 << 20) / len(message) { // about two megabytes: rotates at least once
		log.Add("info", "", "", message)
	}
	log.CloseFile()
	rotated, _ := filepath.Glob(filepath.Join(directory, "events-*.log"))
	if len(rotated) == 0 {
		t.Fatal("the file was not rotated")
	}
	for _, name := range append(rotated, path) {
		if info, err := os.Stat(name); err != nil || info.Size() > 1<<20 {
			t.Fatalf("%s: %v, size limit not kept", name, err)
		}
	}
	reloaded := newEventLog(bus)
	if err := reloaded.OpenFile(path, 1); err != nil {
		t.Fatal(err)
	}
	if count := len(reloaded.NewestFirst("", EventLogCapacity)); count != EventLogCapacity {
		t.Fatalf("loaded %d events, want the newest %d", count, EventLogCapacity)
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
