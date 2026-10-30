package main

// The event log: everything Delta reports (starts, stops, connects, errors,
// alerts). The newest events are kept in memory for the web interface, and
// every event is also appended to a log file, so the history survives a
// restart. The file has one JSON object per line and is rotated when it
// gets too big (the old part is kept as "<file>.1").

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ID            uint64    `json:"id"`
	Time          time.Time `json:"t"`
	Level         string    `json:"level"` // info | warn | error
	ForwarderID   string    `json:"fid,omitempty"`
	ForwarderName string    `json:"name,omitempty"`
	Message       string    `json:"msg"`
}

const eventLogCapacity = 5000 // events kept in memory (and loaded from the file at startup)

// EventLog keeps the newest events in memory (a ring buffer), appends every
// event to the log file, writes it to the process log and pushes it to the browsers.
type EventLog struct {
	mu     sync.Mutex
	events []Event // ring buffer: the event with ID n is stored at index (n-1) % eventLogCapacity
	lastID uint64
	bus    *EventBus

	// The log file. file is nil when the file log is switched off or could not be opened.
	file         *os.File
	filePath     string
	fileSize     int64
	maxFileSize  int64
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
	if len(log.events) < eventLogCapacity {
		log.events = append(log.events, event)
	} else {
		log.events[(event.ID-1)%eventLogCapacity] = event
	}
}

// NewestFirst returns up to limit events, newest first. If forwarderID is
// set, only events of that forwarder are returned.
func (log *EventLog) NewestFirst(forwarderID string, limit int) []Event {
	log.mu.Lock()
	defer log.mu.Unlock()
	result := []Event{}
	for id := log.lastID; id > 0 && log.lastID-id < uint64(len(log.events)) && len(result) < limit; id-- {
		if event := log.events[(id-1)%eventLogCapacity]; forwarderID == "" || event.ForwarderID == forwarderID {
			result = append(result, event)
		}
	}
	return result
}

// ---- the log file ----

// OpenFile switches the file log to path (empty = off). The newest events of
// the file (and of its rotated part) are loaded into memory if nothing has
// been logged yet, i.e. at startup. An error leaves the previous file in use.
func (log *EventLog) OpenFile(path string, maxFileSize int64) error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if path == log.filePath && log.file != nil {
		log.maxFileSize = maxFileSize
		return nil
	}
	if path == "" {
		log.closeFileLocked()
		log.filePath, log.maxFileSize = "", maxFileSize
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return fmt.Errorf("event log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("event log: %w", err)
	}
	if log.lastID == 0 {
		log.loadFromFilesLocked(path)
	}
	log.closeFileLocked()
	log.file, log.filePath, log.fileSize, log.maxFileSize, log.writeFailing = file, path, info.Size(), maxFileSize, false
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

// loadFromFilesLocked reads the rotated part and then the current file, keeps
// the newest eventLogCapacity events in memory and continues their numbering.
// Lines that cannot be parsed (e.g. cut off by a crash) are skipped.
func (log *EventLog) loadFromFilesLocked(path string) {
	var loaded []Event
	for _, name := range []string{path + ".1", path} {
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
			var event Event
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Message != "" {
				loaded = append(loaded, event)
			}
		}
		file.Close()
	}
	if len(loaded) > eventLogCapacity {
		loaded = loaded[len(loaded)-eventLogCapacity:]
	}
	// Renumber them 1…n: the IDs only have to be unique and increasing in this process.
	for _, event := range loaded {
		log.lastID++
		event.ID = log.lastID
		log.rememberLocked(event)
	}
}

// appendToFileLocked writes one event as a JSON line and rotates the file
// when it would grow beyond maxFileSize. log.mu must be held.
func (log *EventLog) appendToFileLocked(event Event) {
	if log.file == nil {
		return
	}
	line, _ := json.Marshal(event)
	line = append(line, '\n')
	if log.maxFileSize > 0 && log.fileSize > 0 && log.fileSize+int64(len(line)) > log.maxFileSize {
		log.rotateLocked()
		if log.file == nil {
			return
		}
	}
	written, err := log.file.Write(line)
	log.fileSize += int64(written)
	if err != nil {
		if !log.writeFailing {
			slog.Error("event log: write failed", "file", log.filePath, "err", err)
		}
		log.writeFailing = true
		return
	}
	log.writeFailing = false
}

// rotateLocked renames the full file to "<file>.1" (replacing an older one)
// and starts a new, empty file.
func (log *EventLog) rotateLocked() {
	log.file.Close()
	log.file = nil
	if err := os.Rename(log.filePath, log.filePath+".1"); err != nil {
		slog.Error("event log: rotation failed", "file", log.filePath, "err", err)
	}
	file, err := os.OpenFile(log.filePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		slog.Error("event log: cannot reopen file", "file", log.filePath, "err", err)
		return
	}
	log.file, log.fileSize = file, 0
}

// FilePath returns the log file in use ("" when the file log is off).
func (log *EventLog) FilePath() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.filePath
}

// resolveEventLogPath turns the configured event_log value into a file path:
// "off" switches the file log off, a relative path is resolved against the
// directory of the config file.
func resolveEventLogPath(configPath, configured string) string {
	configured = strings.TrimSpace(configured)
	if strings.EqualFold(configured, eventLogOff) {
		return ""
	}
	if configured == "" {
		configured = defaultEventLog
	}
	if !filepath.IsAbs(configured) {
		configured = filepath.Join(filepath.Dir(configPath), configured)
	}
	return configured
}
