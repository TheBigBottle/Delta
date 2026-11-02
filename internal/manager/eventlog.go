package manager

// The event log: everything Delta reports (starts, stops, connects, errors,
// alerts). The newest events are kept in memory for the web interface, and
// every event is also appended to a log file, so the history survives a
// restart. The file is rotated when it gets too big (by lumberjack; the old
// part is kept as "<name>-<timestamp><ext>").
//
// The file uses a syslog-style text format (timestamp as in RFC 5424), one
// event per line, so it can be read with less, grep or any log tool:
//
//	2026-10-30T20:37:39.059+01:00 myhost delta[1234]: INFO Delta 1.3.0 started
//	2026-10-30T20:37:41.112+01:00 myhost delta[1234]: ERROR [f3a9c2e17b04] GPS bridge: connect 10.0.0.5:4001: connection refused
//
// Events of a forwarder carry its ID in brackets and its name before the colon.

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"delta/internal/config"
)

type Event struct {
	ID            uint64    `json:"id"`
	Time          time.Time `json:"t"`
	Level         string    `json:"level"` // info | warn | error
	ForwarderID   string    `json:"fid,omitempty"`
	ForwarderName string    `json:"name,omitempty"`
	Message       string    `json:"msg"`
}

const EventLogCapacity = 5000 // events kept in memory (and loaded from the file at startup)

// EventLog keeps the newest events in memory (a ring buffer), appends every
// event to the log file, writes it to the process log and pushes it to the browsers.
type EventLog struct {
	mu     sync.Mutex
	events []Event // ring buffer: the event with ID n is stored at index (n-1) % EventLogCapacity
	lastID uint64
	bus    *EventBus

	// The log file, written and rotated by lumberjack. nil when the file log
	// is switched off or could not be opened.
	file         *lumberjack.Logger
	writeFailing bool // set after a failed write, so a full disk is reported only once
}

func newEventLog(bus *EventBus) *EventLog { return &EventLog{bus: bus} }

func (log *EventLog) Add(level, forwarderID, forwarderName, message string) {
	log.mu.Lock()
	log.lastID++
	event := Event{ID: log.lastID, Time: time.Now(), Level: level, ForwarderID: forwarderID, ForwarderName: forwarderName, Message: message}
	log.rememberLocked(event)
	log.appendToFileLocked(event)
	log.mu.Unlock()

	attrs := []any{"forwarder", forwarderName}
	switch level {
	case "error":
		slog.Error(message, attrs...)
	case "warn":
		slog.Warn(message, attrs...)
	default:
		slog.Info(message, attrs...)
	}
	log.bus.Broadcast("event", event)
}

// rememberLocked stores the event in the ring buffer. log.mu must be held.
func (log *EventLog) rememberLocked(event Event) {
	if len(log.events) < EventLogCapacity {
		log.events = append(log.events, event)
	} else {
		log.events[(event.ID-1)%EventLogCapacity] = event
	}
}

// NewestFirst returns up to limit events, newest first. If forwarderID is
// set, only events of that forwarder are returned.
func (log *EventLog) NewestFirst(forwarderID string, limit int) []Event {
	log.mu.Lock()
	defer log.mu.Unlock()
	result := []Event{}
	for id := log.lastID; id > 0 && log.lastID-id < uint64(len(log.events)) && len(result) < limit; id-- {
		if event := log.events[(id-1)%EventLogCapacity]; forwarderID == "" || event.ForwarderID == forwarderID {
			result = append(result, event)
		}
	}
	return result
}

// ---- the log file ----

// OpenFile switches the file log to path (empty = off). The newest events of
// the file (and of its rotated part) are loaded into memory if nothing has
// been logged yet, i.e. at startup. An error leaves the previous file in use.
func (log *EventLog) OpenFile(path string, maxMegabytes int) error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.file != nil && path == log.file.Filename {
		log.file.MaxSize = maxMegabytes // read by lumberjack on the next write, which also happens under log.mu
		return nil
	}
	if path == "" {
		log.closeFileLocked()
		return nil
	}
	// lumberjack opens the file only on the first write. Open it once here,
	// so a wrong path or missing permission is reported to the user right away.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	probe, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	probe.Close()
	if log.lastID == 0 {
		log.loadFromFilesLocked(path)
	}
	log.closeFileLocked()
	// lumberjack appends to the file and, when the next line would make it
	// bigger than MaxSize megabytes, renames it to "<name>-<timestamp><ext>"
	// and starts a new one. MaxBackups 1 keeps only the newest rotated part,
	// so the log never uses more than about twice MaxSize.
	log.file = &lumberjack.Logger{Filename: path, MaxSize: maxMegabytes, MaxBackups: 1, LocalTime: true}
	log.writeFailing = false
	return nil
}

// CloseFile closes the log file (at shutdown).
func (log *EventLog) CloseFile() {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.closeFileLocked()
}

func (log *EventLog) closeFileLocked() {
	if log.file != nil {
		log.file.Close()
		log.file = nil
	}
}

// rotatedLogFiles returns the rotated parts of the log file at path, oldest
// first: "<file>.1" as written by Delta before 1.6, then the parts lumberjack
// renamed to "<name>-<timestamp><ext>" (the timestamp sorts by time).
func rotatedLogFiles(path string) []string {
	files := []string{path + ".1"}
	extension := filepath.Ext(path)
	prefix := strings.TrimSuffix(path, extension) + "-"
	candidates, _ := filepath.Glob(prefix + "*" + extension) // sorted by name
	for _, candidate := range candidates {
		timestamp := strings.TrimSuffix(strings.TrimPrefix(candidate, prefix), extension)
		if _, err := time.Parse(rotatedFileTimeFormat, timestamp); err == nil { // skip unrelated files
			files = append(files, candidate)
		}
	}
	return files
}

// rotatedFileTimeFormat is the timestamp lumberjack puts into the name of a rotated file.
const rotatedFileTimeFormat = "2006-01-02T15-04-05.000"

// loadFromFilesLocked reads the rotated parts and then the current file, keeps
// the newest EventLogCapacity events in memory and continues their numbering.
// Lines that cannot be parsed (e.g. cut off by a crash) are skipped.
func (log *EventLog) loadFromFilesLocked(path string) {
	var loaded []Event
	for _, name := range append(rotatedLogFiles(path), path) {
		file, err := os.Open(name)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("event log: cannot read old events", "file", name, "err", err)
			}
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			if event, ok := parseLogLine(scanner.Text()); ok {
				loaded = append(loaded, event)
			}
		}
		file.Close()
	}
	if len(loaded) > EventLogCapacity {
		loaded = loaded[len(loaded)-EventLogCapacity:]
	}
	// Renumber them 1…n: the IDs only have to be unique and increasing in this process.
	for _, event := range loaded {
		log.lastID++
		event.ID = log.lastID
		log.rememberLocked(event)
	}
}

// appendToFileLocked writes one event as a line of the log file (lumberjack
// rotates the file when needed). log.mu must be held.
func (log *EventLog) appendToFileLocked(event Event) {
	if log.file == nil {
		return
	}
	if _, err := log.file.Write([]byte(formatLogLine(event))); err != nil {
		if !log.writeFailing { // report a full disk once, not for every event
			slog.Error("event log: write failed", "file", log.file.Filename, "err", err)
		}
		log.writeFailing = true
		return
	}
	log.writeFailing = false
}

// FilePath returns the log file in use ("" when the file log is off).
func (log *EventLog) FilePath() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.file == nil {
		return ""
	}
	return log.file.Filename
}

// ---- the line format ----

const logTimeFormat = "2006-01-02T15:04:05.000Z07:00" // RFC 3339 with milliseconds, as rsyslog writes it

// The host name and process ID in every line, as in syslog.
var logHostName, logProcessTag = func() (string, string) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return strings.Fields(host)[0], "delta[" + strconv.Itoa(os.Getpid()) + "]:"
}()

// Syslog severity names used in the file, and the event levels they stand for.
var levelToSeverity = map[string]string{"info": "INFO", "warn": "WARNING", "error": "ERROR"}
var severityToLevel = map[string]string{"INFO": "info", "WARNING": "warn", "ERROR": "error"}

// oneLine replaces line breaks, so every event stays on one line.
func oneLine(text string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
}

// formatLogLine returns the event as one line of the log file, including the newline.
func formatLogLine(event Event) string {
	severity := levelToSeverity[event.Level]
	if severity == "" {
		severity = "INFO"
	}
	var line strings.Builder
	line.WriteString(event.Time.Format(logTimeFormat) + " " + logHostName + " " + logProcessTag + " " + severity + " ")
	if event.ForwarderID != "" {
		line.WriteString("[" + event.ForwarderID + "] " + oneLine(event.ForwarderName) + ": ")
	}
	line.WriteString(oneLine(event.Message) + "\n")
	return line.String()
}

// parseLogLine reads a line written by formatLogLine. Lines in another format
// are skipped (ok = false). A forwarder name containing ": " cannot be told
// apart from the message; the rest of it then ends up in the message.
func parseLogLine(line string) (event Event, ok bool) {
	// timestamp host tag severity rest
	fields := strings.SplitN(line, " ", 5)
	if len(fields) < 5 {
		return Event{}, false
	}
	timestamp, err := time.Parse(logTimeFormat, fields[0])
	level, knownSeverity := severityToLevel[fields[3]]
	if err != nil || !knownSeverity || !strings.HasPrefix(fields[2], "delta[") {
		return Event{}, false
	}
	event = Event{Time: timestamp, Level: level, Message: fields[4]}
	if rest, isForwarder := strings.CutPrefix(fields[4], "["); isForwarder {
		if id, afterID, found := strings.Cut(rest, "] "); found {
			if name, message, found := strings.Cut(afterID, ": "); found {
				event.ForwarderID, event.ForwarderName, event.Message = id, name, message
			}
		}
	}
	return event, event.Message != ""
}

// resolveEventLogPath turns the configured event_log value into a file path:
// "off" switches the file log off, a relative path is resolved against the
// directory of the config file.
func resolveEventLogPath(configPath, configured string) string {
	configured = strings.TrimSpace(configured)
	if strings.EqualFold(configured, config.EventLogOff) {
		return ""
	}
	if configured == "" {
		configured = config.DefaultEventLog
	}
	return config.ResolvePath(configPath, configured)
}
