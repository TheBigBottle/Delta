package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease serves a GitHub-like "latest release" whose binary is a shell
// script that prints "delta <reportedVersion>". checksumOverride replaces the
// correct checksum when set.
func fakeRelease(t *testing.T, tag, reportedVersion, checksumOverride string) *httptest.Server {
	t.Helper()
	binary := []byte("#!/bin/sh\necho delta " + reportedVersion + "\n")
	sum := sha256.Sum256(binary)
	checksum := hex.EncodeToString(sum[:])
	if checksumOverride != "" {
		checksum = checksumOverride
	}
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag, "html_url": server.URL + "/page",
			"assets": []map[string]string{
				{"name": AssetName(), "browser_download_url": server.URL + "/binary"},
				{"name": "SHA256SUMS", "browser_download_url": server.URL + "/sums"},
			},
		})
	})
	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) { w.Write(binary) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0000  delta-other-arch\n" + checksum + "  " + AssetName() + "\n"))
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// newTestUpdater points an updater at server and at a fake "running" binary
// in a temp directory.
func newTestUpdater(t *testing.T, server *httptest.Server, currentVersion string) (*Updater, string) {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "delta")
	if err := os.WriteFile(executable, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := New(currentVersion)
	updater.ReleaseURL, updater.ExecutablePath = server.URL+"/latest", executable
	return updater, executable
}

func TestInstallReplacesBinary(t *testing.T) {
	server := fakeRelease(t, "v9.9.9", "9.9.9", "")
	updater, executable := newTestUpdater(t, server, "1.7.2")
	release, err := updater.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "9.9.9" || !updater.IsNewer(release) {
		t.Fatalf("got version %q, newer=%v", release.Version, updater.IsNewer(release))
	}
	if _, err := updater.Install(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	installed, _ := os.ReadFile(executable)
	if !strings.Contains(string(installed), "echo delta 9.9.9") {
		t.Fatalf("binary was not replaced: %q", installed)
	}
	if kept, _ := os.ReadFile(executable + ".old"); string(kept) != "old binary" {
		t.Fatalf("old binary not kept: %q", kept)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(executable), ".delta-update-*"))
	if len(leftovers) > 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestInstallRejectsBadDownloads(t *testing.T) {
	for _, testCase := range []struct {
		name, reportedVersion, checksum, wantError string
	}{
		{"wrong checksum", "9.9.9", strings.Repeat("ab", 32), "checksum"},
		{"wrong version", "1.0.0", "", "reports"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := fakeRelease(t, "v9.9.9", testCase.reportedVersion, testCase.checksum)
			updater, executable := newTestUpdater(t, server, "1.7.2")
			release, err := updater.Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = updater.Install(context.Background(), release)
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("got error %v, want one containing %q", err, testCase.wantError)
			}
			if current, _ := os.ReadFile(executable); string(current) != "old binary" {
				t.Fatal("binary was changed although the update failed")
			}
		})
	}
}

func TestIsNewer(t *testing.T) {
	for _, testCase := range []struct {
		current, latest string
		want            bool
	}{
		{"1.7.2", "1.7.3", true},
		{"1.7.2", "1.10.0", true}, // numeric, not text comparison
		{"1.7.2", "1.7.2", false},
		{"1.8.0", "1.7.9", false},
		{"dev", "9.9.9", false}, // development builds are never updated
	} {
		updater := &Updater{CurrentVersion: testCase.current}
		if got := updater.IsNewer(Release{Version: testCase.latest}); got != testCase.want {
			t.Errorf("IsNewer(%s -> %s) = %v, want %v", testCase.current, testCase.latest, got, testCase.want)
		}
	}
}

// TestInstallNeedsWritableFolder: a binary in a folder Delta cannot write to
// (e.g. root-owned /usr/local/bin) gets a clear error and stays untouched.
func TestInstallNeedsWritableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to read-only folders")
	}
	server := fakeRelease(t, "v9.9.9", "9.9.9", "")
	updater, executable := newTestUpdater(t, server, "1.7.2")
	folder := filepath.Dir(executable)
	os.Chmod(folder, 0o555)
	defer os.Chmod(folder, 0o755) // so the temp dir can be removed
	release, err := updater.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = updater.Install(context.Background(), release)
	if err == nil || !strings.Contains(err.Error(), "write access") {
		t.Fatalf("got error %v, want one about write access", err)
	}
	t.Log(err)
	if current, _ := os.ReadFile(executable); string(current) != "old binary" {
		t.Fatal("binary was changed")
	}
}

// A binary built with VERSION=v9.9.9 prints "delta v9.9.9"; that is the same
// version as the tag v9.9.9 and must be accepted.
func TestInstallAcceptsVersionWithV(t *testing.T) {
	server := fakeRelease(t, "v9.9.9", "v9.9.9", "")
	updater, _ := newTestUpdater(t, server, "1.7.2")
	release, err := updater.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updater.Install(context.Background(), release); err != nil {
		t.Fatal(err)
	}
}
