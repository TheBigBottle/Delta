package manager

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"delta/internal/config"
	"delta/internal/updater"
)

func newUpdateTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(dir + "/delta.json")
	if err != nil {
		t.Fatal(err)
	}
	m := New(dir+"/delta.json", cfg)
	m.Version, m.Updater = "1.7.2", updater.New("1.7.2")
	return m
}

// Automatic install needs the check: switching the check off must also
// switch automatic install off, whichever fields the request contains.
func TestUpdateSettingsNeverAutoInstallWithoutCheck(t *testing.T) {
	m := newUpdateTestManager(t)
	on, off := true, false
	if err := m.UpdateSettings(SettingsUpdate{UpdateCheck: &on, UpdateAutoInstall: &on}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateSettings(SettingsUpdate{UpdateCheck: &off}); err != nil {
		t.Fatal(err)
	}
	if settings := m.Settings(); settings["update_auto_install"] != false {
		t.Fatalf("auto install still on after the check was switched off: %v", settings)
	}
	if err := m.UpdateSettings(SettingsUpdate{UpdateAutoInstall: &on}); err != nil {
		t.Fatal(err)
	}
	if settings := m.Settings(); settings["update_auto_install"] != false {
		t.Fatal("auto install switched on while the check is off")
	}
}

// A check while another check or install runs reports ErrUpdateBusy, so the
// background checker does not mistake it for a successful check.
func TestCheckWhileBusyReportsBusy(t *testing.T) {
	m := newUpdateTestManager(t)
	m.update.status.State = "installing"
	if _, err := m.CheckForUpdate(context.Background()); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("got %v, want ErrUpdateBusy", err)
	}
}

// A setup request without the update fields keeps an "updates" block that
// was written into the config file before the first start.
func TestSetupKeepsPreseededUpdateSettings(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/delta.json"
	if err := os.WriteFile(path, []byte(`{"updates": {"check": true, "auto_install": true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(path, cfg)
	if _, err := m.CompleteSetup(config.InitialSetup{Password: "first-password"}); err != nil {
		t.Fatal(err)
	}
	if settings := m.Settings(); settings["update_check"] != true || settings["update_auto_install"] != true {
		t.Fatalf("pre-seeded update settings were reset: %v", settings)
	}
}

// An automatic install whose download is still running when a capture starts
// installs the binary, waits ("pending") until the capture has ended, and
// only then asks main to restart.
func TestAutomaticInstallWaitsForCaptureBeforeRestart(t *testing.T) {
	binary := []byte("#!/bin/sh\necho delta 9.9.9\n")
	checksum := sha256.Sum256(binary)
	releaseDownload := make(chan struct{})
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v9.9.9", "assets": []map[string]string{
			{"name": updater.AssetName(), "browser_download_url": server.URL + "/binary"},
			{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums"},
		}})
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%x  %s\n", checksum, updater.AssetName())
	})
	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
		<-releaseDownload // the download "takes" until the capture has started
		w.Write(binary)
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	recordingPollInterval = 20 * time.Millisecond
	defer func() { recordingPollInterval = 10 * time.Second }()
	m := newUpdateTestManager(t)
	m.Updater.ReleaseURL = server.URL + "/latest"
	m.Updater.ExecutablePath = filepath.Join(t.TempDir(), "delta")
	os.WriteFile(m.Updater.ExecutablePath, []byte("old"), 0o755)
	if _, err := m.CheckForUpdate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.startInstall(true); err != nil {
		t.Fatal(err)
	}

	// A capture starts while the download runs.
	inst := newInstance(config.ForwarderConfig{ID: "f1", Name: "test"})
	inst.capture = &captureJob{stopSignal: make(chan struct{})}
	m.mu.Lock()
	m.instances["f1"], m.instanceOrder = inst, append(m.instanceOrder, "f1")
	m.mu.Unlock()
	close(releaseDownload)

	waitForUpdateState(t, m, "pending")
	select {
	case <-m.RestartRequests():
		t.Fatal("restarted while the capture was running")
	case <-time.After(200 * time.Millisecond):
	}
	m.mu.Lock()
	inst.capture = nil // the capture ends
	m.mu.Unlock()
	select {
	case <-m.RestartRequests():
	case <-time.After(15 * time.Second):
		t.Fatal("no restart after the capture ended")
	}
}

func waitForUpdateState(t *testing.T, m *Manager, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for m.UpdateStatus().State != want {
		if time.Now().After(deadline) {
			t.Fatalf("update state is %q, want %q (error: %s)", m.UpdateStatus().State, want, m.UpdateStatus().Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
