// Package updater finds a newer Delta release on GitHub and replaces the
// running binary with it.
//
// How an update is installed safely:
//
//  1. Ask the GitHub API for the latest release (drafts and pre-releases are
//     never returned by /releases/latest) and compare its version.
//  2. Download the binary for this platform (delta-linux-<arch>) next to the
//     running one, and its line of the release's SHA256SUMS file.
//  3. Reject the download unless the checksum matches.
//  4. Run the new file with -version: it must start on this machine and
//     report exactly the expected version.
//  5. Keep the running binary as "<binary>.old" (for a manual rollback) and
//     rename the new one into place. Renames in one directory are atomic, so
//     there is never a half-written binary.
//
// Restarting into the new binary is the caller's job (see cmd/delta).
package updater

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultReleaseURL is the GitHub API address of Delta's latest release.
const DefaultReleaseURL = "https://api.github.com/repos/TheBigBottle/Delta/releases/latest"

const (
	checksumFileName = "SHA256SUMS"
	maxBinarySize    = 100 << 20 // refuse anything bigger; Delta is about 10 MB
	versionRunLimit  = 10 * time.Second
)

// Release is the latest published release.
type Release struct {
	Version string            `json:"version"` // e.g. "1.8.0" (the tag without its "v")
	URL     string            `json:"url"`     // release page on GitHub, shown in the UI
	assets  map[string]string // asset file name -> download URL
}

// Updater checks for and installs updates. The zero value is not usable;
// create it with New.
type Updater struct {
	ReleaseURL     string       // GitHub API URL of the latest release (tests point it elsewhere)
	CurrentVersion string       // the running version, e.g. "1.7.2"
	ExecutablePath string       // the binary to replace; empty = the running executable
	client         *http.Client // timeouts come from the request contexts
}

func New(currentVersion string) *Updater {
	return &Updater{ReleaseURL: DefaultReleaseURL, CurrentVersion: currentVersion, client: &http.Client{}}
}

// AssetName is the release file that fits this machine, e.g. "delta-linux-arm64".
// It follows the names scripts/build.sh gives the cross-compiled binaries.
func AssetName() string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return "delta-" + runtime.GOOS + "-" + arch
}

// Latest asks GitHub for the latest release.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.ReleaseURL, nil)
	if err != nil {
		return Release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Delta/"+u.CurrentVersion) // GitHub rejects requests without one
	response, err := u.client.Do(request)
	if err != nil {
		return Release{}, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered %s", response.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("unexpected answer from GitHub: %w", err)
	}
	release := Release{Version: strings.TrimPrefix(body.TagName, "v"), URL: body.HTMLURL, assets: map[string]string{}}
	if _, ok := parseVersion(release.Version); !ok {
		return Release{}, fmt.Errorf("latest release has no valid version (%q)", body.TagName)
	}
	for _, asset := range body.Assets {
		release.assets[asset.Name] = asset.DownloadURL
	}
	return release, nil
}

// IsNewer reports whether the release is newer than the running version.
// A running version that is not a plain "x.y.z" (e.g. a development build)
// is never updated automatically, so it counts as not older.
func (u *Updater) IsNewer(release Release) bool {
	latest, latestOK := parseVersion(release.Version)
	current, currentOK := parseVersion(u.CurrentVersion)
	if !latestOK || !currentOK {
		return false
	}
	for i := range latest {
		if latest[i] != current[i] {
			return latest[i] > current[i]
		}
	}
	return false
}

// IsRelease reports whether version is a release number ("x.y.z"). Builds
// without one (e.g. "dev", from scripts/build.sh without VERSION) are never
// replaced by a release, so a self-built binary is not overwritten.
func IsRelease(version string) bool {
	_, ok := parseVersion(version)
	return ok
}

// parseVersion reads "1.7.2" (or "v1.7.2") into its three numbers.
func parseVersion(text string) ([3]int, bool) {
	var numbers [3]int
	parts := strings.Split(strings.TrimPrefix(text, "v"), ".")
	if len(parts) != 3 {
		return numbers, false
	}
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return numbers, false
		}
		numbers[i] = number
	}
	return numbers, true
}

// Install downloads the release's binary for this machine, checks it and puts
// it in place of the running binary. It returns the path of the replaced
// binary. On any error the running binary is left untouched.
func (u *Updater) Install(ctx context.Context, release Release) (string, error) {
	assetName := AssetName()
	binaryURL, checksumURL := release.assets[assetName], release.assets[checksumFileName]
	if binaryURL == "" {
		return "", fmt.Errorf("release %s has no binary for this machine (%s)", release.Version, assetName)
	}
	if checksumURL == "" {
		return "", fmt.Errorf("release %s has no %s file, so the download cannot be verified", release.Version, checksumFileName)
	}
	targetPath, err := u.executablePath()
	if err != nil {
		return "", err
	}

	expectedChecksum, err := u.fetchChecksum(ctx, checksumURL, assetName)
	if err != nil {
		return "", err
	}

	// The new binary is written next to the old one, so the final rename stays
	// inside one file system (and fails early if the directory is read-only).
	directory := filepath.Dir(targetPath)
	newFile, err := os.CreateTemp(directory, ".delta-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot write to %s (%w). Delta needs write access to the folder of its binary to update itself", directory, errors.Unwrap(err))
	}
	newPath := newFile.Name()
	defer os.Remove(newPath) // no-op after the successful rename

	actualChecksum, err := u.download(ctx, binaryURL, newFile)
	if err == nil {
		// On disk before the rename: otherwise a power loss shortly after the
		// update can leave an empty binary behind (SD cards, ext4 delayed allocation).
		err = newFile.Sync()
	}
	if closeErr := newFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if actualChecksum != expectedChecksum {
		return "", fmt.Errorf("checksum of %s does not match %s; the download was not installed", assetName, checksumFileName)
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		return "", err
	}
	if err := checkRunsAsVersion(ctx, newPath, release.Version); err != nil {
		return "", err
	}

	// Keep the running binary as ".old" for a manual rollback: a second name
	// for the same file (hard link), or a copy where links are not possible.
	// Then one rename puts the new binary in place. The target path always
	// holds a complete binary, even if Delta is killed in between.
	oldPath := targetPath + ".old"
	if err := keepCopy(targetPath, oldPath); err != nil {
		return "", fmt.Errorf("cannot keep the old binary as %s: %w", oldPath, err)
	}
	if err := os.Rename(newPath, targetPath); err != nil {
		return "", fmt.Errorf("cannot replace %s: %w", targetPath, err)
	}
	syncDirectory(directory) // make the renames themselves survive a power loss
	return targetPath, nil
}

// syncDirectory flushes a folder's entries (e.g. a rename) to disk.
func syncDirectory(path string) {
	if folder, err := os.Open(path); err == nil {
		folder.Sync()
		folder.Close()
	}
}

// RemoveLeftovers deletes temporary files of an install that was cut short
// (Delta stopped during the download). Called once at startup. Only files
// older than any install can take are removed, so another Delta instance
// using the same binary folder keeps its download.
func (u *Updater) RemoveLeftovers() {
	targetPath, err := u.executablePath()
	if err != nil {
		return
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(targetPath), ".delta-update-*"))
	for _, leftover := range leftovers {
		if info, err := os.Stat(leftover); err == nil && time.Since(info.ModTime()) > 15*time.Minute {
			os.Remove(leftover)
		}
	}
}

// keepCopy makes oldPath a copy of path (replacing an older one): a hard
// link if the file system allows it, otherwise a real copy.
func keepCopy(path, oldPath string) error {
	if err := os.Remove(oldPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if os.Link(path, oldPath) == nil {
		return nil
	}
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	copyFile, err := os.OpenFile(oldPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(copyFile, source)
	if closeErr := copyFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(oldPath)
	}
	return err
}

// executablePath is the binary to replace, with symbolic links resolved, so a
// link such as /usr/local/bin/delta keeps pointing to the updated file.
func (u *Updater) executablePath() (string, error) {
	path := u.ExecutablePath
	if path == "" {
		var err error
		if path, err = os.Executable(); err != nil {
			return "", fmt.Errorf("cannot find the running binary: %w", err)
		}
	}
	return filepath.EvalSymlinks(path)
}

// fetchChecksum reads the expected SHA-256 of assetName from SHA256SUMS
// (lines of "<hex>  <file name>", as written by sha256sum).
func (u *Updater) fetchChecksum(ctx context.Context, url, assetName string) (string, error) {
	var sums bytes.Buffer
	if _, err := u.download(ctx, url, &limitedWriter{writer: &sums, remaining: 1 << 20}); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(&sums)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s has no entry for %s", checksumFileName, assetName)
}

// download copies url into destination and returns the SHA-256 of the data.
func (u *Updater) download(ctx context.Context, url string, destination io.Writer) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "Delta/"+u.CurrentVersion)
	response, err := u.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download of %s failed: %s", filepath.Base(url), response.Status)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(response.Body, maxBinarySize+1))
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	if written > maxBinarySize {
		return "", fmt.Errorf("download of %s is larger than %d MB", filepath.Base(url), maxBinarySize>>20)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// checkRunsAsVersion starts the new binary with -version. It must print
// "delta <version>"; this catches a binary for the wrong CPU or a broken file.
func checkRunsAsVersion(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, versionRunLimit)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run on this machine: %w", err)
	}
	// "delta v1.8.0" and "delta 1.8.0" are the same version.
	got := strings.TrimSpace(string(output))
	if strings.Replace(got, "delta v", "delta ", 1) != "delta "+strings.TrimPrefix(version, "v") {
		return fmt.Errorf("the downloaded binary reports %q instead of \"delta %s\"", got, version)
	}
	return nil
}

// limitedWriter fails once more than remaining bytes are written; it keeps a
// broken or hostile server from filling the memory with a huge SHA256SUMS.
type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("file is too large")
	}
	w.remaining -= int64(len(data))
	return w.writer.Write(data)
}
