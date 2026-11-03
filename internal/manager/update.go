package manager

// The update check: looks for a newer Delta release on GitHub (at startup and
// once a day, when switched on in the settings), shows it in the web
// interface, and installs it on request or automatically. Installing replaces
// the binary (see internal/updater) and then asks main to restart Delta.

import (
	"context"
	"errors"
	"time"

	"delta/internal/updater"
)

const (
	firstUpdateCheckDelay = 30 * time.Second // after startup, so the network is up
	updateCheckInterval   = 24 * time.Hour
	updateRetryInterval   = time.Hour // after a failed check
	updateCheckTimeout    = 30 * time.Second
	updateInstallTimeout  = 10 * time.Minute // the download is about 10 MB
)

// recordingPollInterval is how often a postponed restart checks whether the
// captures and replays have ended (a variable, so tests can shorten it).
var recordingPollInterval = 10 * time.Second

// UpdateStatus is the state of the update check, shown under Settings and
// sent to the browsers as the "update" live event whenever it changes.
type UpdateStatus struct {
	Current     string     `json:"current"`               // running version
	Development bool       `json:"development,omitempty"` // Current is no release number (self-built); never updated
	Latest      string     `json:"latest,omitempty"`      // newest release found
	Available   bool       `json:"available"`             // Latest is newer than Current
	ReleaseURL  string     `json:"release_url,omitempty"` // release page on GitHub
	CheckedAt   *time.Time `json:"checked_at,omitempty"`  // last successful check
	State       string     `json:"state"`                 // "idle", "checking", "installing", "pending" (installed, restart waits for a capture/replay) or "restarting"
	Error       string     `json:"error,omitempty"`       // why the last check or install failed
}

// updateState is guarded by Manager.updateMu, not m.mu, because checks and
// downloads take seconds and must not block the rest of the manager.
type updateState struct {
	status      UpdateStatus
	release     updater.Release // the release behind status.Latest
	nextCheckAt time.Time       // when the background checker checks next
}

// UpdateStatus returns the current state of the update check.
func (m *Manager) UpdateStatus() UpdateStatus {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	return m.updateStatusLocked()
}

func (m *Manager) updateStatusLocked() UpdateStatus {
	status := m.update.status
	status.Current, status.Development = m.Version, !updater.IsRelease(m.Version)
	if status.State == "" {
		status.State = "idle"
	}
	return status
}

// setUpdateStatus changes the state under updateMu and tells the browsers.
func (m *Manager) setUpdateStatus(change func(status *UpdateStatus)) UpdateStatus {
	m.updateMu.Lock()
	change(&m.update.status)
	status := m.updateStatusLocked()
	m.updateMu.Unlock()
	m.LiveUpdates.Broadcast("update", status)
	return status
}

// RestartRequests delivers the path of the new binary once an update is
// installed. main then shuts down cleanly and starts that binary.
func (m *Manager) RestartRequests() <-chan string { return m.restartRequests }

// ErrUpdateBusy means a check or install is already running.
var ErrUpdateBusy = errors.New("an update check or install is already running")

// CheckForUpdate asks GitHub for the latest release now. It works whether or
// not the automatic check is switched on (it is the "Check now" button).
func (m *Manager) CheckForUpdate(ctx context.Context) (UpdateStatus, error) {
	if m.Updater == nil {
		return m.UpdateStatus(), errors.New("updates are not available in this build")
	}
	m.updateMu.Lock()
	if state := m.update.status.State; state != "" && state != "idle" {
		m.updateMu.Unlock()
		return m.UpdateStatus(), ErrUpdateBusy
	}
	m.update.status.State = "checking"
	m.updateMu.Unlock()
	m.LiveUpdates.Broadcast("update", m.UpdateStatus())

	ctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	release, err := m.Updater.Latest(ctx)

	m.updateMu.Lock()
	wasKnown := m.update.status.Latest == release.Version
	m.updateMu.Unlock()
	status := m.setUpdateStatus(func(status *UpdateStatus) {
		status.State = "idle"
		if err != nil {
			status.Error = err.Error()
			return
		}
		now := time.Now()
		status.Error, status.CheckedAt = "", &now
		status.Latest, status.ReleaseURL, status.Available = release.Version, release.URL, m.Updater.IsNewer(release)
		m.update.release = release
	})
	if err != nil {
		m.Events.Add("warn", "", "", "update check failed: "+err.Error())
		return status, err
	}
	if status.Available && !wasKnown {
		m.Events.Add("info", "", "", "Delta "+release.Version+" is available (running "+m.Version+")")
	}
	return status, nil
}

// InstallUpdate downloads and installs the release found by the last check,
// in the background (the "Install" button). Progress and errors arrive as
// "update" live events and in the event log. On success Delta restarts into
// the new version right away; the user was told that captures and replays end.
func (m *Manager) InstallUpdate() error { return m.startInstall(false) }

// startInstall starts an install. An automatic one (isAutomatic) restarts only
// when no capture or replay is running, also one started during the download.
func (m *Manager) startInstall(isAutomatic bool) error {
	if m.Updater == nil {
		return errors.New("updates are not available in this build")
	}
	m.updateMu.Lock()
	if !m.update.status.Available {
		m.updateMu.Unlock()
		return errors.New("no newer version found; check for updates first")
	}
	if state := m.update.status.State; state != "" && state != "idle" {
		m.updateMu.Unlock()
		return ErrUpdateBusy
	}
	m.update.status.State, m.update.status.Error = "installing", ""
	release := m.update.release
	m.updateMu.Unlock()
	m.LiveUpdates.Broadcast("update", m.UpdateStatus())
	// A plain goroutine, not backgroundJobs: Shutdown must not wait for a
	// download. Stopping in the middle leaves the old binary in place.
	go m.installRelease(release, isAutomatic)
	return nil
}

func (m *Manager) installRelease(release updater.Release, isAutomatic bool) {
	m.Events.Add("info", "", "", "installing Delta "+release.Version+" …")
	ctx, cancel := context.WithTimeout(context.Background(), updateInstallTimeout)
	defer cancel()
	binaryPath, err := m.Updater.Install(ctx, release)
	if err != nil {
		m.Events.Add("error", "", "", "update to "+release.Version+" failed: "+err.Error())
		m.setUpdateStatus(func(status *UpdateStatus) { status.State, status.Error = "idle", err.Error() })
		return
	}
	m.Events.Add("info", "", "", "Delta "+release.Version+" installed (previous binary kept as "+binaryPath+".old)")
	// The new binary is in place; an automatic install only restarts once no
	// capture or replay would be cut short. If Delta is stopped meanwhile, the
	// new version simply starts next time.
	if isAutomatic && m.hasActiveRecording() {
		m.Events.Add("info", "", "", "restart postponed until the running captures and replays have ended")
		m.setUpdateStatus(func(status *UpdateStatus) { status.State = "pending" })
		for m.hasActiveRecording() {
			time.Sleep(recordingPollInterval)
		}
	}
	m.Events.Add("info", "", "", "restarting into Delta "+release.Version)
	m.setUpdateStatus(func(status *UpdateStatus) { status.State = "restarting" })
	select {
	case m.restartRequests <- binaryPath:
	default: // a restart is already pending
	}
}

// RunUpdateChecker checks for updates in the background while the setting is
// on: first shortly after startup, then once a day (hourly after a failure).
// With automatic install on, a newer release is installed right away, unless
// a capture or replay is running; then it waits for the next round.
func (m *Manager) RunUpdateChecker(stop <-chan struct{}) {
	if m.Updater == nil {
		return
	}
	m.updateMu.Lock()
	m.update.nextCheckAt = time.Now().Add(firstUpdateCheckDelay)
	m.updateMu.Unlock()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-m.checkUpdateNow:
		case <-ticker.C:
			m.updateMu.Lock()
			due := time.Now().After(m.update.nextCheckAt)
			m.updateMu.Unlock()
			if !due {
				continue
			}
		}
		m.mu.Lock()
		settings := m.config.Updates
		m.mu.Unlock()
		if !settings.Check {
			continue
		}
		status, err := m.CheckForUpdate(context.Background())
		if errors.Is(err, ErrUpdateBusy) {
			continue // e.g. a manual install is running; try again on a later tick
		}
		m.updateMu.Lock()
		m.update.nextCheckAt = time.Now().Add(updateCheckInterval)
		if err != nil {
			m.update.nextCheckAt = time.Now().Add(updateRetryInterval)
		}
		m.updateMu.Unlock()
		if err != nil || !status.Available || !settings.AutoInstall {
			continue
		}
		if m.hasActiveRecording() {
			m.Events.Add("info", "", "", "automatic update to "+status.Latest+" postponed: a capture or replay is running")
			m.updateMu.Lock()
			m.update.nextCheckAt = time.Now().Add(updateRetryInterval)
			m.updateMu.Unlock()
			continue
		}
		if err := m.startInstall(true); err != nil {
			m.Events.Add("warn", "", "", "automatic update: "+err.Error())
		}
	}
}

// requestUpdateCheck makes the background checker check soon (e.g. right
// after the setting was switched on). It never blocks.
func (m *Manager) requestUpdateCheck() {
	select {
	case m.checkUpdateNow <- struct{}{}:
	default:
	}
}

// hasActiveRecording reports whether any capture or replay is running, which
// a restart would cut short.
func (m *Manager) hasActiveRecording() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		if inst.capture != nil || inst.replay != nil {
			return true
		}
	}
	return false
}
