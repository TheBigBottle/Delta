package main

// This file defines the configuration file format and how it is loaded,
// validated and saved. Delta keeps everything in one JSON file: the web
// interface settings and the list of forwarders. The file is rewritten
// whenever something changes in the web interface.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults used when the config file leaves a value empty.
const (
	defaultWebPort      = 8470
	defaultDataDir      = "delta-data"
	defaultSessionHours = 24
	defaultDownAfterSec = 30
)

// Config is the content of the config file.
type Config struct {
	Port int    `json:"port"` // web interface port
	Bind string `json:"bind"` // web interface bind address, empty = all interfaces
	// PasswordHash is the hashed login password (see password.go). Empty means
	// no password has been set yet: the web interface then asks for one.
	PasswordHash string `json:"password_hash,omitempty"`
	// Password may be written into the file by hand in plain text, e.g. to
	// reset a forgotten password. Delta hashes it on startup and removes it.
	Password     string            `json:"password,omitempty"`
	DataDir      string            `json:"data_dir"`      // where captures are stored (relative to the config file)
	SessionHours int               `json:"session_hours"` // how long a login stays valid
	Alerts       AlertConfig       `json:"alerts"`
	Forwarders   []ForwarderConfig `json:"forwarders"`
}

type AlertConfig struct {
	WebhookURL       string `json:"webhook_url"` // receives a JSON POST when an alert fires or resolves
	DownAfterSeconds int    `json:"down_after"`  // how long a forwarder must be unhealthy before alerting
}

// ForwarderConfig describes one forwarder: one input whose data is copied to all outputs.
type ForwarderConfig struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Group            string           `json:"group,omitempty"`
	Tags             []string         `json:"tags"`
	Comment          string           `json:"comment,omitempty"`    // free-text notes, shown in the UI only
	Framing          string           `json:"framing"`              // "raw" (bytes as they arrive) or "lines" (one message per line, e.g. NMEA)
	Autostart        bool             `json:"autostart"`            // true while the forwarder is meant to run; restored after a restart
	IdleAlertSeconds int              `json:"idle_alert,omitempty"` // alert when no input data arrives for this long, 0 = off
	Input            EndpointConfig   `json:"input"`
	Outputs          []EndpointConfig `json:"outputs"`
	Created          time.Time        `json:"created"`
}

// EndpointConfig describes the input or one output of a forwarder.
type EndpointConfig struct {
	Name                 string `json:"name,omitempty"` // optional label shown in the UI
	Mode                 string `json:"mode"`           // see inputModes / outputModes
	Host                 string `json:"host,omitempty"` // remote host, or the local bind address for listen modes
	Port                 int    `json:"port"`
	MaxClients           int    `json:"max_clients,omitempty"`    // tcp-listen only, 0 = unlimited
	QueueSize            int    `json:"queue,omitempty"`          // outputs: buffered messages (per client for tcp-listen), 0 = default
	Hello                string `json:"hello,omitempty"`          // udp-connect input: datagram that announces us to the remote host
	HelloIntervalSeconds int    `json:"hello_interval,omitempty"` // udp-connect input: how often the hello is repeated
	// tcp-connect only: more remote hosts besides Host:Port. Delta keeps one
	// connection to each; an input merges their data, an output sends a copy to each.
	Targets []RemoteTarget `json:"targets,omitempty"`
}

// RemoteTarget is one additional remote host of a tcp-connect endpoint.
type RemoteTarget struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// maxConnectTargets limits the remote hosts of one tcp-connect endpoint.
const maxConnectTargets = 64

// The modes an endpoint can have. "listen" modes open a local port,
// "connect"/"send" modes reach out to a remote host.
var (
	inputModes  = map[string]bool{"tcp-listen": true, "tcp-connect": true, "udp-listen": true, "udp-connect": true}
	outputModes = map[string]bool{"tcp-listen": true, "tcp-connect": true, "udp-send": true}
)

// address returns "host:port", the form the net package expects.
func (ep EndpointConfig) address() string { return net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)) }

// connectAddresses returns every remote address of a tcp-connect endpoint:
// Host:Port first, then the additional targets.
func (ep EndpointConfig) connectAddresses() []string {
	addresses := []string{ep.address()}
	for _, target := range ep.Targets {
		addresses = append(addresses, net.JoinHostPort(target.Host, strconv.Itoa(target.Port)))
	}
	return addresses
}

// queueSizeOr returns the configured queue size, or fallback if none is set.
func (ep EndpointConfig) queueSizeOr(fallback int) int {
	if ep.QueueSize > 0 {
		return ep.QueueSize
	}
	return fallback
}

// isListener reports whether the endpoint opens a local port (tcp-listen, udp-listen).
func (ep EndpointConfig) isListener() bool { return strings.HasSuffix(ep.Mode, "-listen") }

// validate checks an endpoint and clears settings that do not apply to its mode.
func (ep *EndpointConfig) validate(isInput bool) error {
	ep.Mode = strings.TrimSpace(ep.Mode)
	ep.Host = strings.TrimSpace(ep.Host)
	ep.Name = strings.TrimSpace(ep.Name)
	allowedModes := outputModes
	if isInput {
		allowedModes = inputModes
	}
	if !allowedModes[ep.Mode] {
		return fmt.Errorf("unknown mode %q", ep.Mode)
	}
	if ep.Port < 1 || ep.Port > 65535 {
		return errors.New("port must be 1-65535")
	}
	if !ep.isListener() && ep.Host == "" {
		return fmt.Errorf("host is required for mode %s", ep.Mode)
	}
	if ep.MaxClients < 0 || ep.QueueSize < 0 || ep.QueueSize > 1_000_000 || ep.HelloIntervalSeconds < 0 {
		return errors.New("invalid limits")
	}
	if ep.Mode != "tcp-listen" {
		ep.MaxClients = 0
	}
	if ep.Mode != "udp-connect" {
		ep.Hello, ep.HelloIntervalSeconds = "", 0
	}
	if isInput {
		ep.QueueSize = 0 // inputs have no queue
	}
	if ep.Mode != "tcp-connect" {
		ep.Targets = nil
	}
	if len(ep.Targets) > maxConnectTargets {
		return fmt.Errorf("at most %d additional hosts", maxConnectTargets)
	}
	seenAddresses := map[string]bool{ep.address(): true}
	for i := range ep.Targets {
		target := &ep.Targets[i]
		target.Host = strings.TrimSpace(target.Host)
		if target.Host == "" {
			return fmt.Errorf("host %d: host is required", i+2)
		}
		if target.Port < 1 || target.Port > 65535 {
			return fmt.Errorf("host %d: port must be 1-65535", i+2)
		}
		address := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
		if seenAddresses[address] {
			return fmt.Errorf("%s is listed twice", address)
		}
		seenAddresses[address] = true
	}
	return nil
}

// maxCommentLength limits the notes of a forwarder, so the config file and the
// live updates (which carry the comment every second) stay small.
const maxCommentLength = 4000

// normalize validates a forwarder, trims text fields, removes duplicate tags
// and fills in the ID and creation time if they are missing.
func (fc *ForwarderConfig) normalize() error {
	fc.Name = strings.TrimSpace(fc.Name)
	fc.Group = strings.TrimSpace(fc.Group)
	fc.Comment = strings.TrimSpace(fc.Comment)
	if fc.Name == "" {
		return errors.New("name is required")
	}
	if len(fc.Comment) > maxCommentLength {
		return fmt.Errorf("comment is too long (at most %d characters)", maxCommentLength)
	}
	if fc.ID == "" {
		fc.ID = newForwarderID()
	}
	switch fc.Framing {
	case "":
		fc.Framing = "raw"
	case "raw", "lines":
	default:
		return fmt.Errorf("unknown framing %q", fc.Framing)
	}

	tags := []string{} // never nil, so the JSON always contains a list
	seenTags := map[string]bool{}
	for _, tag := range fc.Tags {
		tag = strings.TrimSpace(tag)
		if tag != "" && !seenTags[tag] {
			seenTags[tag] = true
			tags = append(tags, tag)
		}
	}
	fc.Tags = tags

	if fc.IdleAlertSeconds < 0 {
		fc.IdleAlertSeconds = 0
	}
	if err := fc.Input.validate(true); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	if len(fc.Outputs) == 0 {
		return errors.New("at least one output is required")
	}
	for i := range fc.Outputs {
		if err := fc.Outputs[i].validate(false); err != nil {
			return fmt.Errorf("output %d: %w", i+1, err)
		}
	}
	if fc.Created.IsZero() {
		fc.Created = time.Now().UTC()
	}
	return nil
}

func defaultConfig() *Config {
	return &Config{
		Port:         defaultWebPort,
		DataDir:      defaultDataDir,
		SessionHours: defaultSessionHours,
		Alerts:       AlertConfig{DownAfterSeconds: defaultDownAfterSec},
		Forwarders:   []ForwarderConfig{},
	}
}

// loadConfig reads and validates the config file. If the file does not exist
// it is created (without a password; the web interface asks for one).
// A plain text password found in the file is replaced by its hash.
func loadConfig(path string) (cfg *Config, err error) {
	cfg = defaultConfig()
	data, err := os.ReadFile(path)
	fileIsNew := errors.Is(err, fs.ErrNotExist)
	if err != nil && !fileIsNew {
		return nil, err
	}
	if !fileIsNew {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields() // catch typos in hand-edited files
		if err := decoder.Decode(cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	passwordWasPlainText := cfg.Password != ""
	if passwordWasPlainText {
		if err := checkNewPassword(cfg.Password); err != nil {
			return nil, fmt.Errorf("%s: password: %w", path, err)
		}
		if cfg.PasswordHash, err = hashPassword(cfg.Password); err != nil {
			return nil, err
		}
		cfg.Password = ""
	}
	if cfg.Port == 0 {
		cfg.Port = defaultWebPort
	}
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir
	}
	if cfg.SessionHours <= 0 {
		cfg.SessionHours = defaultSessionHours
	}
	if cfg.Alerts.DownAfterSeconds <= 0 {
		cfg.Alerts.DownAfterSeconds = defaultDownAfterSec
	}
	if cfg.Forwarders == nil {
		cfg.Forwarders = []ForwarderConfig{}
	}

	seenIDs := map[string]bool{}
	for i := range cfg.Forwarders {
		fc := &cfg.Forwarders[i]
		if err := fc.normalize(); err != nil {
			return nil, fmt.Errorf("%s: forwarder %q: %w", path, fc.Name, err)
		}
		if seenIDs[fc.ID] {
			return nil, fmt.Errorf("%s: duplicate forwarder id %q", path, fc.ID)
		}
		seenIDs[fc.ID] = true
	}

	if fileIsNew || passwordWasPlainText {
		if err := saveConfig(path, cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// saveConfig writes the config atomically: it writes a temporary file next to
// the real one and renames it, so a crash never leaves a half-written file.
// The file is only readable by its owner (it contains the password hash).
func saveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".delta-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name()) // no-op after a successful rename

	writeErr := func() error {
		if _, err := tmpFile.Write(append(data, '\n')); err != nil {
			return err
		}
		if err := tmpFile.Chmod(0o600); err != nil {
			return err
		}
		return tmpFile.Sync() // make sure the data is on disk before the rename
	}()
	if closeErr := tmpFile.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	return os.Rename(tmpFile.Name(), path)
}

func newForwarderID() string { return "f" + randomHex(6) }

// randomHex returns n random bytes as a hex string (2n characters).
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
