package main

// The Manager owns all forwarders. It creates, starts, stops and deletes them,
// writes every change to the config file, samples traffic rates once per
// second, evaluates alerts and pushes the live state to the web interface.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

var errNotFound = errors.New("forwarder not found")

const (
	historyLength      = 600              // seconds of rate history kept per forwarder (10 minutes)
	startRetryDelay    = 15 * time.Second // a forwarder that failed to start is retried this often
	maxEventsPerSecond = 20               // per forwarder; protects the event log from reconnect storms
)

// Instance is one configured forwarder plus its runtime state. It exists
// whether or not the forwarder is running.
type Instance struct {
	config       ForwarderConfig
	running      *Forwarder // nil while stopped
	state        string     // "stopped", "running" or "error" (failed to start)
	startError   string
	stateSince   time.Time
	nextRetryAt  time.Time // when a failed start is tried again
	rates        Rates     // traffic per second, computed by the sampler
	lastCounters trafficCounters
	lastSampleAt time.Time
	history      rateHistory
	alerts       map[string]*alertState // keyed by alert kind: "down", "idle"
	capture      *captureJob            // active recording, if any
	replay       *replayJob             // active replay, if any
	eventLimit   eventRateLimiter
}

type Rates struct {
	BytesIn  float64 `json:"in"`
	BytesOut float64 `json:"out"`
	Messages float64 `json:"msgs"`
}

type alertState struct {
	conditionSince time.Time // when the alert condition became true; zero while false
	firing         bool
	reason         string // latest explanation, shown in the UI while firing
}

// eventRateLimiter allows maxEventsPerSecond events per second and counts the rest.
type eventRateLimiter struct {
	mu         sync.Mutex
	second     int64 // unix second of the current window
	count      int
	suppressed int
}

// ForwarderStatus is what the API returns for a forwarder: its configuration
// plus the live state. The embedded config fields appear at the top level of the JSON.
type ForwarderStatus struct {
	ForwarderConfig
	State        string            `json:"state"`  // stopped | running | error
	Health       string            `json:"health"` // stopped | ok | degraded | error
	Error        string            `json:"error,omitempty"`
	StateSince   time.Time         `json:"since"`
	Rates        Rates             `json:"rate"`
	BytesIn      uint64            `json:"bytes_in"`
	BytesOut     uint64            `json:"bytes_out"`
	Messages     uint64            `json:"chunks"`
	Drops        uint64            `json:"drops"`
	Connections  int               `json:"clients"`             // ClientsIn + ClientsOut
	ClientsIn    int               `json:"clients_in"`          // sources connected to the input (TCP clients, UDP senders, the remote host)
	ClientsOut   int               `json:"clients_out"`         // receivers connected to the outputs
	LastDataAt   int64             `json:"last_data,omitempty"` // unix milliseconds
	InputStatus  *EndpointStatus   `json:"in_status,omitempty"`
	OutputStatus []EndpointStatus  `json:"out_status,omitempty"`
	FiringAlerts []string          `json:"firing,omitempty"`
	AlertReasons map[string]string `json:"alert_reasons,omitempty"` // alert kind -> why it fires
	Capture      *CaptureStatus    `json:"capture,omitempty"`
	Replay       *ReplayStatus     `json:"replay,omitempty"`
}

type Manager struct {
	mu            sync.Mutex // guards config, instances, instanceOrder and all Instance fields
	configPath    string
	config        *Config
	dataDir       string
	instances     map[string]*Instance
	instanceOrder []string // forwarder IDs in creation order, as stored in the config file
	events        *EventLog
	liveUpdates   *EventBus // server-sent events to all open browser tabs
	httpClient    *http.Client
	webAddress    string // effective web interface address (command line flags may override the config)
}

func newManager(configPath string, config *Config) *Manager {
	dataDir := resolveNextToConfig(configPath, config.DataDir)
	bus := &EventBus{subscribers: map[chan []byte]struct{}{}}
	m := &Manager{
		configPath: configPath, config: config, dataDir: dataDir,
		instances: map[string]*Instance{}, liveUpdates: bus,
		events:     newEventLog(bus),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	// Without a usable log file Delta still runs; the events are then kept in memory only.
	if err := m.events.OpenFile(resolveEventLogPath(configPath, config.EventLog), int64(config.EventLogMaxMB)<<20); err != nil {
		m.events.Add("error", "", "", err.Error()+" (events are kept in memory only)")
	}
	for _, fc := range config.Forwarders {
		m.instances[fc.ID] = newInstance(fc)
		m.instanceOrder = append(m.instanceOrder, fc.ID)
	}
	return m
}

func newInstance(fc ForwarderConfig) *Instance {
	return &Instance{config: fc, state: "stopped", stateSince: time.Now(), alerts: map[string]*alertState{}}
}

// saveLocked writes the config file. m.mu must be held.
func (m *Manager) saveLocked() {
	m.config.Forwarders = make([]ForwarderConfig, 0, len(m.instanceOrder))
	for _, id := range m.instanceOrder {
		m.config.Forwarders = append(m.config.Forwarders, m.instances[id].config)
	}
	if err := saveConfig(m.configPath, m.config); err != nil {
		m.events.Add("error", "", "", "saving config failed: "+err.Error())
	}
}

// StartAutostartForwarders starts every forwarder that was running when Delta last stopped.
func (m *Manager) StartAutostartForwarders() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.instanceOrder {
		if inst := m.instances[id]; inst.config.Autostart {
			m.startLocked(inst)
		}
	}
}

// eventLoggerFor returns the function a running forwarder uses to write to
// the event log. It limits each forwarder to maxEventsPerSecond events, so a
// client that reconnects in a tight loop cannot flood the log.
func (m *Manager) eventLoggerFor(inst *Instance) func(level, message string) {
	id, name := inst.config.ID, inst.config.Name
	limiter := &inst.eventLimit
	return func(level, message string) {
		limiter.mu.Lock()
		now := time.Now().Unix()
		if now != limiter.second {
			if limiter.suppressed > 0 {
				m.events.Add("warn", id, name, fmt.Sprintf("%d events suppressed", limiter.suppressed))
			}
			limiter.second, limiter.count, limiter.suppressed = now, 0, 0
		}
		limiter.count++
		if limiter.count > maxEventsPerSecond {
			limiter.suppressed++
			limiter.mu.Unlock()
			return
		}
		limiter.mu.Unlock()
		m.events.Add(level, id, name, message)
	}
}

// startLocked starts a forwarder. On failure the instance goes into the
// "error" state and the sampler retries it. m.mu must be held.
func (m *Manager) startLocked(inst *Instance) error {
	if inst.running != nil {
		return nil
	}
	fw, err := startForwarder(inst.config, m.eventLoggerFor(inst))
	inst.stateSince = time.Now()
	if err != nil {
		// Log a failed retry only when the reason changes, not every 15 seconds.
		if inst.state != "error" || inst.startError != err.Error() {
			m.events.Add("error", inst.config.ID, inst.config.Name, "start failed: "+err.Error())
		}
		inst.state, inst.startError = "error", err.Error()
		inst.nextRetryAt = time.Now().Add(startRetryDelay)
		return err
	}
	inst.running, inst.state, inst.startError = fw, "running", ""
	inst.lastCounters, inst.lastSampleAt = trafficCounters{}, time.Now()
	m.events.Add("info", inst.config.ID, inst.config.Name, "started")
	return nil
}

// stopLocked detaches the running forwarder and resets the instance state.
// Stopping a forwarder can take a moment, so the caller must call Stop on the
// returned forwarder after releasing m.mu (see stopForwarders).
func (m *Manager) stopLocked(inst *Instance) *Forwarder {
	fw := inst.running
	inst.running = nil
	if inst.capture != nil {
		inst.capture.stop()
	}
	if inst.replay != nil {
		inst.replay.stop()
	}
	if fw != nil || inst.state == "error" {
		m.events.Add("info", inst.config.ID, inst.config.Name, "stopped")
	}
	inst.state, inst.startError, inst.stateSince, inst.rates = "stopped", "", time.Now(), Rates{}
	for _, alert := range inst.alerts {
		alert.conditionSince, alert.firing = time.Time{}, false // a stopped forwarder has no alerts
	}
	return fw
}

// stopForwarders stops several forwarders in parallel and waits for all of them.
func stopForwarders(forwarders ...*Forwarder) {
	var wg sync.WaitGroup
	for _, fw := range forwarders {
		if fw != nil {
			wg.Go(fw.Stop)
		}
	}
	wg.Wait()
}

// Create adds a new forwarder and optionally starts it. If starting fails,
// the forwarder is still created; the returned status shows the error.
func (m *Manager) Create(fc ForwarderConfig, start bool) (ForwarderStatus, error) {
	fc.ID, fc.Created, fc.Autostart = "", time.Time{}, start
	if err := fc.normalize(); err != nil {
		return ForwarderStatus{}, err
	}
	m.mu.Lock()
	inst := newInstance(fc)
	m.instances[fc.ID] = inst
	m.instanceOrder = append(m.instanceOrder, fc.ID)
	m.events.Add("info", fc.ID, fc.Name, "created")
	var startErr error
	if start {
		startErr = m.startLocked(inst)
	}
	m.saveLocked()
	status := m.statusLocked(inst, true)
	m.mu.Unlock()
	m.BroadcastForwarders()
	return status, startErr
}

// Update replaces a forwarder's configuration. A running forwarder is
// restarted with the new configuration.
func (m *Manager) Update(id string, fc ForwarderConfig) (ForwarderStatus, error) {
	m.mu.Lock()
	inst := m.instances[id]
	if inst == nil {
		m.mu.Unlock()
		return ForwarderStatus{}, errNotFound
	}
	fc.ID, fc.Created, fc.Autostart = id, inst.config.Created, inst.config.Autostart
	if err := fc.normalize(); err != nil {
		m.mu.Unlock()
		return ForwarderStatus{}, err
	}
	wasRunning := inst.running != nil || inst.state == "error"
	oldForwarder := m.stopLocked(inst)
	m.mu.Unlock()
	stopForwarders(oldForwarder) // frees the ports before the new config binds them

	m.mu.Lock()
	inst.config = fc
	m.events.Add("info", id, fc.Name, "configuration changed")
	var startErr error
	if wasRunning {
		startErr = m.startLocked(inst)
	}
	m.saveLocked()
	status := m.statusLocked(inst, true)
	m.mu.Unlock()
	m.BroadcastForwarders()
	return status, startErr
}

// Delete stops and removes forwarders. Their captures are kept.
func (m *Manager) Delete(ids ...string) error {
	m.mu.Lock()
	var toStop []*Forwarder
	for _, id := range ids {
		inst := m.instances[id]
		if inst == nil {
			m.mu.Unlock()
			return errNotFound
		}
		toStop = append(toStop, m.stopLocked(inst))
		delete(m.instances, id)
		m.instanceOrder = slices.DeleteFunc(m.instanceOrder, func(other string) bool { return other == id })
		m.events.Add("info", id, inst.config.Name, "deleted")
	}
	m.saveLocked()
	m.mu.Unlock()
	stopForwarders(toStop...)
	m.BroadcastForwarders()
	return nil
}

// SetRunning starts, stops or restarts forwarders. It also stores whether
// each forwarder should run after Delta restarts (the autostart flag).
func (m *Manager) SetRunning(action string, ids ...string) error {
	m.mu.Lock()
	var toStop []*Forwarder
	for _, id := range ids {
		inst := m.instances[id]
		if inst == nil {
			m.mu.Unlock()
			return errNotFound
		}
		if action == "stop" || action == "restart" {
			toStop = append(toStop, m.stopLocked(inst))
		}
		inst.config.Autostart = action != "stop"
	}
	m.saveLocked()
	m.mu.Unlock()
	stopForwarders(toStop...)

	var startErrors []error
	if action != "stop" {
		m.mu.Lock()
		for _, id := range ids {
			if inst := m.instances[id]; inst != nil {
				if err := m.startLocked(inst); err != nil {
					startErrors = append(startErrors, fmt.Errorf("%s: %w", inst.config.Name, err))
				}
			}
		}
		m.mu.Unlock()
	}
	m.BroadcastForwarders()
	return errors.Join(startErrors...)
}

// UpdateTags adds a tag ("tag") to or removes a tag ("untag") from several forwarders at once.
func (m *Manager) UpdateTags(action, value string, ids ...string) error {
	m.mu.Lock()
	defer m.BroadcastForwarders() // runs after the unlock below (defers run in reverse order)
	defer m.mu.Unlock()
	for _, id := range ids {
		inst := m.instances[id]
		if inst == nil {
			return errNotFound
		}
		fc := &inst.config
		switch action {
		case "tag":
			if value != "" && !slices.Contains(fc.Tags, value) {
				fc.Tags = append(fc.Tags, value)
			}
		case "untag":
			fc.Tags = slices.DeleteFunc(fc.Tags, func(tag string) bool { return tag == value })
		}
	}
	m.saveLocked()
	return nil
}

// DisconnectPeer closes one connection of a running forwarder.
func (m *Manager) DisconnectPeer(id, endpoint string, peerID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[id]
	return inst != nil && inst.running != nil && inst.running.disconnectPeer(endpoint, peerID)
}

// RunningForwarder returns the running forwarder with this ID, or nil.
func (m *Manager) RunningForwarder(id string) *Forwarder {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst := m.instances[id]; inst != nil {
		return inst.running
	}
	return nil
}

// Get returns one forwarder including the list of connected peers.
func (m *Manager) Get(id string) (ForwarderStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[id]
	if inst == nil {
		return ForwarderStatus{}, false
	}
	return m.statusLocked(inst, true), true
}

func (m *Manager) List() []ForwarderStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked()
}

func (m *Manager) listLocked() []ForwarderStatus {
	list := make([]ForwarderStatus, 0, len(m.instanceOrder))
	for _, id := range m.instanceOrder {
		list = append(list, m.statusLocked(m.instances[id], false))
	}
	return list
}

// statusLocked builds the API view of an instance. m.mu must be held.
func (m *Manager) statusLocked(inst *Instance, includePeers bool) ForwarderStatus {
	status := ForwarderStatus{
		ForwarderConfig: inst.config, State: inst.state, Error: inst.startError,
		StateSince: inst.stateSince, Rates: inst.rates,
	}
	if status.Tags == nil {
		status.Tags = []string{} // the web interface expects a list, never null
	}
	switch {
	case inst.state == "error":
		status.Health = "error"
	case inst.running == nil:
		status.Health = "stopped"
	default:
		// Running: "degraded" if any endpoint is not connected/listening.
		status.Health = "ok"
		input, outputs := inst.running.snapshot(includePeers)
		status.InputStatus, status.OutputStatus = &input, outputs
		totals := inst.running.counters()
		status.BytesIn, status.BytesOut, status.Messages, status.Drops = totals.BytesIn, totals.BytesOut, totals.Messages, totals.Drops
		status.ClientsIn = input.PeerCount
		if !isHealthyState(input.State) {
			status.Health = "degraded"
		}
		for _, output := range outputs {
			status.ClientsOut += output.PeerCount
			if !isHealthyState(output.State) {
				status.Health = "degraded"
			}
		}
		status.Connections = status.ClientsIn + status.ClientsOut
		if lastData := inst.running.lastDataAt.Load(); lastData > 0 {
			status.LastDataAt = lastData / int64(time.Millisecond)
		}
	}
	for kind, alert := range inst.alerts {
		if alert.firing {
			status.FiringAlerts = append(status.FiringAlerts, kind)
			if status.AlertReasons == nil {
				status.AlertReasons = map[string]string{}
			}
			status.AlertReasons[kind] = alert.reason
		}
	}
	slices.Sort(status.FiringAlerts)
	if inst.capture != nil {
		captureStatus := inst.capture.status()
		status.Capture = &captureStatus
	}
	if inst.replay != nil {
		replayStatus := inst.replay.status()
		status.Replay = &replayStatus
	}
	return status
}

// History returns the rate history (last 10 minutes) of a forwarder.
func (m *Manager) History(id string) (map[string]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[id]
	if inst == nil {
		return nil, false
	}
	return inst.history.series(), true
}

// BroadcastForwarders sends the current forwarder list to all open browser tabs.
func (m *Manager) BroadcastForwarders() {
	m.liveUpdates.Broadcast("forwarders", m.List())
}

// RunSampler calls sample once per second until stop is closed.
func (m *Manager) RunSampler(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			m.sample(now)
		}
	}
}

// sample computes the traffic rates, records them in the history, evaluates
// alerts, retries failed starts and pushes the new state to the browsers.
func (m *Manager) sample(now time.Time) {
	m.mu.Lock()
	for _, id := range m.instanceOrder {
		inst := m.instances[id]
		if fw := inst.running; fw != nil {
			totals := fw.counters()
			if seconds := now.Sub(inst.lastSampleAt).Seconds(); seconds > 0 {
				inst.rates = Rates{
					BytesIn:  float64(totals.BytesIn-inst.lastCounters.BytesIn) / seconds,
					BytesOut: float64(totals.BytesOut-inst.lastCounters.BytesOut) / seconds,
					Messages: float64(totals.Messages-inst.lastCounters.Messages) / seconds,
				}
			}
			inst.lastCounters, inst.lastSampleAt = totals, now
		} else if inst.state == "error" && inst.config.Autostart && now.After(inst.nextRetryAt) {
			m.startLocked(inst) // e.g. the port was busy or the network was not up yet at boot
		}
		inst.history.add(now.UnixMilli(), inst.rates)
		m.evaluateAlertsLocked(inst, now)
	}
	list := m.listLocked()
	m.mu.Unlock()
	m.liveUpdates.Broadcast("forwarders", list)
}

// ---- alerts ----

// evaluateAlertsLocked checks the two alert kinds of an instance:
//   - "down": failed to start, or an endpoint has been unhealthy for DownAfterSeconds
//   - "idle": running, but no input data for IdleAlertSeconds
func (m *Manager) evaluateAlertsLocked(inst *Instance, now time.Time) {
	var downReason string
	switch {
	case inst.state == "error":
		downReason = inst.startError
	case inst.running != nil:
		input, outputs := inst.running.snapshot(false)
		var unhealthy []string
		if !isHealthyState(input.State) {
			unhealthy = append(unhealthy, describeUnhealthyEndpoint("input", input))
		}
		for _, output := range outputs {
			if !isHealthyState(output.State) {
				unhealthy = append(unhealthy, describeUnhealthyEndpoint("output", output))
			}
		}
		downReason = strings.Join(unhealthy, "; ")
	}
	downDelay := time.Duration(m.config.Alerts.DownAfterSeconds) * time.Second
	m.updateAlertLocked(inst, "down", downReason != "", downDelay, now, downReason)

	isIdle := false
	var idleFor time.Duration
	if inst.running != nil && inst.config.IdleAlertSeconds > 0 {
		lastActivity := max(inst.running.lastDataAt.Load(), inst.running.startedAt.UnixNano())
		idleFor = now.Sub(time.Unix(0, lastActivity)).Truncate(time.Second)
		isIdle = idleFor >= time.Duration(inst.config.IdleAlertSeconds)*time.Second
	}
	m.updateAlertLocked(inst, "idle", isIdle, 0, now, fmt.Sprintf("no input data for %s", idleFor))
}

// describeUnhealthyEndpoint explains an endpoint problem for alerts, e.g.
// "output tcp-connect 10.0.0.5:2000 is down (connection refused: …)", or with
// several remote hosts "input tcp-connect: 1 of 3 hosts connected (10.0.0.6:4001: …)".
func describeUnhealthyEndpoint(role string, status EndpointStatus) string {
	text := fmt.Sprintf("%s %s %s is %s", role, status.Mode, status.Address, status.State)
	if len(status.Connections) > 1 {
		connectedCount := 0
		for _, connection := range status.Connections {
			if connection.State == "connected" {
				connectedCount++
			}
		}
		text = fmt.Sprintf("%s %s: %d of %d hosts connected", role, status.Mode, connectedCount, len(status.Connections))
	}
	if status.LastError != "" {
		text += " (" + status.LastError + ")"
	}
	return text
}

// updateAlertLocked fires an alert once its condition has been true for
// delay, and resolves it as soon as the condition is false again.
func (m *Manager) updateAlertLocked(inst *Instance, kind string, conditionTrue bool, delay time.Duration, now time.Time, reason string) {
	alert := inst.alerts[kind]
	if alert == nil {
		alert = &alertState{}
		inst.alerts[kind] = alert
	}
	alert.reason = reason
	if !conditionTrue {
		if alert.firing {
			alert.firing = false
			m.notifyAlert(inst, kind, false, "")
		}
		alert.conditionSince = time.Time{}
		return
	}
	if alert.conditionSince.IsZero() {
		alert.conditionSince = now
	}
	if !alert.firing && now.Sub(alert.conditionSince) >= delay {
		alert.firing = true
		m.notifyAlert(inst, kind, true, reason)
	}
}

// notifyAlert writes the alert to the event log and, if configured, sends it to the webhook.
func (m *Manager) notifyAlert(inst *Instance, kind string, firing bool, reason string) {
	status, level := "RESOLVED", "info"
	text := fmt.Sprintf("[Delta] RESOLVED: %s is no longer %s", inst.config.Name, kind)
	if firing {
		status, level = "FIRING", "error"
		text = fmt.Sprintf("[Delta] FIRING: %s is %s", inst.config.Name, kind)
	}
	if reason != "" {
		text += " — " + reason
	}
	m.events.Add(level, inst.config.ID, inst.config.Name, text)
	if url := m.config.Alerts.WebhookURL; url != "" {
		payload := map[string]any{
			"text": text, "content": text, // "text": Slack/Teams/Mattermost, "content": Discord
			"forwarder": inst.config.Name, "id": inst.config.ID, "alert": kind,
			"status": status, "detail": reason, "time": time.Now().UTC(),
		}
		go m.postWebhook(url, payload) // never block the sampler on the network
	}
}

func (m *Manager) postWebhook(url string, payload any) error {
	body, _ := json.Marshal(payload)
	resp, err := m.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			err = fmt.Errorf("webhook returned %s", resp.Status)
		}
	}
	if err != nil {
		m.events.Add("warn", "", "", "webhook failed: "+err.Error())
	}
	return err
}

// ---- settings & import/export ----

// SettingsUpdate is the request body of PUT /api/settings. Fields that are
// left out are not changed.
type SettingsUpdate struct {
	WebhookURL       *string `json:"webhook_url,omitempty"`
	DownAfterSeconds int     `json:"down_after"`
	EventLog         *string `json:"event_log,omitempty"` // file path, relative to the config file, or "off"
	EventLogMaxMB    int     `json:"event_log_max_mb"`
	NewPassword      string  `json:"password,omitempty"`
	CurrentPassword  string  `json:"current_password,omitempty"`
}

// PasswordHash returns the stored hash ("" if no password is set yet).
func (m *Manager) PasswordHash() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config.PasswordHash
}

// NeedsPasswordSetup is true until the first password has been set.
func (m *Manager) NeedsPasswordSetup() bool { return m.PasswordHash() == "" }

// InitialSetup is the request body of POST /api/setup: the password and the
// first settings, entered on the first-start page. Empty or zero fields keep
// the value from the config file (or its default).
type InitialSetup struct {
	Password         string `json:"password"`
	Port             int    `json:"port"`
	Bind             string `json:"bind"`
	DataDir          string `json:"data_dir"`
	SessionHours     int    `json:"session_hours"`
	WebhookURL       string `json:"webhook_url"`
	DownAfterSeconds int    `json:"down_after"`
	EventLog         string `json:"event_log"`
	EventLogMaxMB    int    `json:"event_log_max_mb"`
}

// SetupDefaults returns the values the first-start page is filled with.
func (m *Manager) SetupDefaults() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{
		"port": m.config.Port, "bind": m.config.Bind, "data_dir": m.config.DataDir,
		"session_hours": m.config.SessionHours, "webhook_url": m.config.Alerts.WebhookURL,
		"down_after": m.config.Alerts.DownAfterSeconds, "event_log": m.config.EventLog,
		"event_log_max_mb": m.config.EventLogMaxMB, "config_path": m.configPath, "web_addr": m.webAddress,
	}
}

// CompleteSetup sets the password and the first settings on first start.
// It fails if a password already exists, so the setup page cannot be used
// to take over. Everything is checked first: either all of it is saved or
// nothing. restartNeeded is true when the web port or bind address changed,
// because the web interface keeps its current address until Delta restarts.
func (m *Manager) CompleteSetup(setup InitialSetup) (restartNeeded bool, err error) {
	if err := checkNewPassword(setup.Password); err != nil {
		return false, err
	}
	if err := validateInitialSetup(&setup); err != nil {
		return false, err
	}
	hash, err := hashPassword(setup.Password) // slow on purpose; done before taking the lock
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config.PasswordHash != "" {
		return false, errors.New("a password is already set")
	}
	if setup.EventLog != "" || setup.EventLogMaxMB > 0 {
		if err := m.switchEventLogLocked(setup.EventLog, setup.EventLogMaxMB); err != nil {
			return false, err
		}
	}
	m.config.PasswordHash = hash
	if setup.Port != 0 && setup.Port != m.config.Port {
		m.config.Port, restartNeeded = setup.Port, true
	}
	if setup.Bind != m.config.Bind {
		m.config.Bind, restartNeeded = setup.Bind, true
	}
	if setup.DataDir != "" {
		// Safe without further locking: until the password is set, every API
		// request is refused, so no capture can be using the old directory.
		m.config.DataDir, m.dataDir = setup.DataDir, resolveNextToConfig(m.configPath, setup.DataDir)
	}
	if setup.SessionHours > 0 {
		m.config.SessionHours = setup.SessionHours
	}
	m.config.Alerts.WebhookURL = setup.WebhookURL
	if setup.DownAfterSeconds > 0 {
		m.config.Alerts.DownAfterSeconds = setup.DownAfterSeconds
	}
	m.saveLocked()
	m.events.Add("info", "", "", "first-start setup completed")
	return restartNeeded, nil
}

// switchEventLogLocked changes the event log file and its rotation size
// (empty / 0 = keep the current value). The new file is opened first; if
// that fails nothing is changed. m.mu must be held.
func (m *Manager) switchEventLogLocked(configured string, maxMB int) error {
	if configured = strings.TrimSpace(configured); configured == "" {
		configured = m.config.EventLog
	}
	if maxMB <= 0 {
		maxMB = m.config.EventLogMaxMB
	}
	path := resolveEventLogPath(m.configPath, configured)
	if err := m.events.OpenFile(path, int64(maxMB)<<20); err != nil {
		return err
	}
	if configured != m.config.EventLog {
		m.events.Add("info", "", "", "event log file: "+cmp.Or(path, "off (memory only)"))
	}
	m.config.EventLog, m.config.EventLogMaxMB = configured, maxMB
	return nil
}

func (m *Manager) SessionHours() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config.SessionHours
}

// Settings returns what the settings page shows (never the password).
func (m *Manager) Settings() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{
		"webhook_url": m.config.Alerts.WebhookURL, "down_after": m.config.Alerts.DownAfterSeconds,
		"config_path": m.configPath, "data_dir": m.dataDir, "version": version,
		"event_log": m.config.EventLog, "event_log_max_mb": m.config.EventLogMaxMB, "event_log_path": m.events.FilePath(),
		"port": m.config.Port, "bind": m.config.Bind, "web_addr": m.webAddress,
	}
}

func (m *Manager) UpdateSettings(update SettingsUpdate) error {
	// Password hashing is deliberately slow, so it happens before taking the lock.
	var newHash string
	if update.NewPassword != "" {
		if !verifyPassword(update.CurrentPassword, m.PasswordHash()) {
			return errors.New("current password is wrong")
		}
		if err := checkNewPassword(update.NewPassword); err != nil {
			return err
		}
		var err error
		if newHash, err = hashPassword(update.NewPassword); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if update.EventLog != nil || update.EventLogMaxMB > 0 {
		configured := ""
		if update.EventLog != nil {
			// An emptied field means "back to the default file".
			configured = cmp.Or(strings.TrimSpace(*update.EventLog), defaultEventLog)
		}
		if err := m.switchEventLogLocked(configured, update.EventLogMaxMB); err != nil {
			return err
		}
	}
	if newHash != "" {
		m.config.PasswordHash = newHash
		m.events.Add("info", "", "", "password changed")
	}
	if update.DownAfterSeconds > 0 {
		m.config.Alerts.DownAfterSeconds = update.DownAfterSeconds
	}
	if update.WebhookURL != nil {
		m.config.Alerts.WebhookURL = strings.TrimSpace(*update.WebhookURL)
	}
	m.saveLocked()
	return nil
}

// Export returns the configuration of the given forwarders (all if ids is empty).
func (m *Manager) Export(ids []string) []ForwarderConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	exported := []ForwarderConfig{}
	for _, id := range m.instanceOrder {
		if len(ids) == 0 || slices.Contains(ids, id) {
			exported = append(exported, m.instances[id].config)
		}
	}
	return exported
}

// Import adds forwarders as new, stopped instances. All of them are validated
// first, so either all or none are imported.
func (m *Manager) Import(configs []ForwarderConfig) (int, error) {
	for i := range configs {
		configs[i].ID, configs[i].Created, configs[i].Autostart = "", time.Time{}, false
		if err := configs[i].normalize(); err != nil {
			return 0, fmt.Errorf("forwarder %d (%s): %w", i+1, configs[i].Name, err)
		}
	}
	m.mu.Lock()
	for _, fc := range configs {
		m.instances[fc.ID] = newInstance(fc)
		m.instanceOrder = append(m.instanceOrder, fc.ID)
	}
	m.events.Add("info", "", "", fmt.Sprintf("imported %d forwarders", len(configs)))
	m.saveLocked()
	m.mu.Unlock()
	m.BroadcastForwarders()
	return len(configs), nil
}

// Shutdown stops all forwarders without changing their autostart flags, so
// they come back the next time Delta starts.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	var toStop []*Forwarder
	for _, inst := range m.instances {
		if inst.capture != nil {
			inst.capture.stop()
		}
		if inst.replay != nil {
			inst.replay.stop()
		}
		toStop = append(toStop, inst.running)
		inst.running = nil
	}
	m.mu.Unlock()
	stopForwarders(toStop...)
	backgroundJobs.Wait() // let captures flush their files
}

// ---- rate history ----

// rateHistory is a ring buffer with one rate sample per second.
type rateHistory struct {
	nextIndex  int
	isFull     bool
	timestamps [historyLength]int64 // unix milliseconds
	samples    [historyLength]Rates
}

func (h *rateHistory) add(timestamp int64, rates Rates) {
	h.timestamps[h.nextIndex], h.samples[h.nextIndex] = timestamp, rates
	h.nextIndex = (h.nextIndex + 1) % historyLength
	if h.nextIndex == 0 {
		h.isFull = true
	}
}

// series returns the history oldest first, as parallel arrays for the chart.
func (h *rateHistory) series() map[string]any {
	count, oldest := h.nextIndex, 0
	if h.isFull {
		count, oldest = historyLength, h.nextIndex
	}
	times := make([]int64, count)
	bytesIn, bytesOut, messages := make([]float64, count), make([]float64, count), make([]float64, count)
	for i := range count {
		j := (oldest + i) % historyLength
		times[i] = h.timestamps[j]
		bytesIn[i], bytesOut[i], messages[i] = h.samples[j].BytesIn, h.samples[j].BytesOut, h.samples[j].Messages
	}
	return map[string]any{"t": times, "in": bytesIn, "out": bytesOut, "msgs": messages}
}

// ---- live updates to the browsers (server-sent events) ----

// EventBus fans out messages to every open /api/stream connection.
type EventBus struct {
	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
}

func (bus *EventBus) Subscribe() chan []byte {
	ch := make(chan []byte, 256)
	bus.mu.Lock()
	bus.subscribers[ch] = struct{}{}
	bus.mu.Unlock()
	return ch
}

func (bus *EventBus) Unsubscribe(ch chan []byte) {
	bus.mu.Lock()
	delete(bus.subscribers, ch)
	bus.mu.Unlock()
}

// Broadcast encodes v once and sends it as a server-sent event named eventName.
func (bus *EventBus) Broadcast(eventName string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	message := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", eventName, data)
	bus.mu.Lock()
	for ch := range bus.subscribers {
		select {
		case ch <- message:
		default: // a slow browser skips this update; the next one comes within a second
		}
	}
	bus.mu.Unlock()
}
