package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// versionCmd prints the current version info.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print Fluctio version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Fluctio %s\n", version)
			fmt.Printf("  commit: %s\n", commit)
			fmt.Printf("  built:  %s\n", date)
			fmt.Printf("  go:     %s\n", runtime.Version())
			fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
}

// upgradeCmd downloads and installs the latest release.
func upgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Upgrade Fluctio to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return doUpgrade()
		},
	}
}

func doUpgrade() error {
	const repo = "fluctio-ai/fluctio"
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	fmt.Println("⚡ Checking for updates...")

	// 1. Fetch latest release info
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to parse release info: %w", err)
	}

	// 2. Check if already up to date
	if release.TagName == version {
		fmt.Printf("✅ Already up to date (%s)\n", version)
		return nil
	}
	fmt.Printf("📦 New version available: %s → %s\n", version, release.TagName)

	// 3. Find the right asset for this platform
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	var suffix string
	if goos == "windows" {
		suffix = fmt.Sprintf("fluctio_%s_%s.zip", goos, goarch)
	} else {
		suffix = fmt.Sprintf("fluctio_%s_%s.tar.gz", goos, goarch)
	}

	var downloadURL, checksumsURL string
	for _, asset := range release.Assets {
		if asset.Name == suffix {
			downloadURL = asset.BrowserDownloadURL
		}
		if asset.Name == "checksums.txt" {
			checksumsURL = asset.BrowserDownloadURL
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no binary found for %s/%s in release %s", goos, goarch, release.TagName)
	}

	fmt.Printf("⬇️  Downloading %s...\n", suffix)

	// 4. Download to temp file
	dlResp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer dlResp.Body.Close()

	tmpFile, err := os.CreateTemp("", "fluctio-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, dlResp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	tmpFile.Close()

	// Verify the archive against the release's checksums.txt before it
	// gets executed or installed. goreleaser publishes the file for every
	// release; a missing checksums file or mismatched hash aborts the
	// upgrade instead of installing unverified bytes.
	if checksumsURL == "" {
		return fmt.Errorf("release %s has no checksums.txt — refusing to install unverified artifacts", release.TagName)
	}
	expected, err := fetchExpectedChecksum(client, checksumsURL, suffix)
	if err != nil {
		return fmt.Errorf("checksum lookup failed: %w", err)
	}
	got, err := fileSHA256(tmpPath)
	if err != nil {
		return fmt.Errorf("checksum failed: %w", err)
	}
	if got != expected {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s — the download may be corrupted or tampered with", suffix, got, expected)
	}
	fmt.Printf("🔒 checksum OK (%s…)\n", got[:12])

	// 5. Extract binary
	tmpDir, err := os.MkdirTemp("", "fluctio-extract-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if goos == "windows" {
		// Unzip
		unzip := exec.Command("powershell", "-Command",
			fmt.Sprintf("Expand-Archive -Path '%s' -DestinationPath '%s' -Force", tmpPath, tmpDir))
		if out, err := unzip.CombinedOutput(); err != nil {
			return fmt.Errorf("unzip failed: %s: %w", string(out), err)
		}
	} else {
		// Untar
		tar := exec.Command("tar", "-xzf", tmpPath, "-C", tmpDir)
		if out, err := tar.CombinedOutput(); err != nil {
			return fmt.Errorf("untar failed: %s: %w", string(out), err)
		}
	}

	// 6. Find current binary path and replace
	currentBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find current binary: %w", err)
	}
	currentBin, _ = filepath.EvalSymlinks(currentBin)

	binaryName := "fluctio"
	if goos == "windows" {
		binaryName = "fluctio.exe"
	}
	newBin := filepath.Join(tmpDir, binaryName)

	if _, err := os.Stat(newBin); err != nil {
		return fmt.Errorf("extracted binary not found at %s", newBin)
	}

	// 7. Replace current binary
	fmt.Printf("📝 Installing to %s...\n", currentBin)

	// Atomic-ish swap: stage + rename. A direct overwrite of the running
	// image fails on Linux (ETXTBSY) and Windows (file lock), which made
	// the self-upgrade dead on both platforms.
	cleanupOld, err := replaceBinary(currentBin, newBin)
	if err != nil {
		if goos != "windows" {
			fmt.Println("🔐 Need elevated permissions, trying sudo...")
			sudo := exec.Command("sudo", "cp", newBin, currentBin)
			sudo.Stdin = os.Stdin
			sudo.Stdout = os.Stdout
			sudo.Stderr = os.Stderr
			if err := sudo.Run(); err != nil {
				return fmt.Errorf("install failed (try: sudo cp %s %s): %w", newBin, currentBin, err)
			}
		} else {
			return fmt.Errorf("install failed: %w", err)
		}
	} else {
		// Best-effort cleanup of the swapped-out image. On Windows the
		// running process still maps the old file, so the delete may fail
		// here; the next successful upgrade removes the leftover.
		cleanupOld()
	}

	fmt.Printf("✅ Upgraded to %s\n", release.TagName)
	return nil
}

// fetchExpectedChecksum downloads a goreleaser checksums.txt and returns
// the expected sha256 for the given asset filename. checksums.txt lines
// are "<hex>  <filename>" (two spaces).
func fetchExpectedChecksum(client *http.Client, checksumsURL, assetName string) (string, error) {
	resp, err := client.Get(checksumsURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("checksums.txt returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[1] == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", assetName)
}

// fileSHA256 returns the hex sha256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// replaceBinary swaps the running executable via a same-directory rename
// chain: stage the new image next to the destination, move the old one
// aside, then move the new one into place. Overwriting the running image
// in place fails on Linux (ETXTBSY) and Windows (image file lock) — the
// direct os.WriteFile approach made the self-upgrade unusable on both.
// The returned func removes the swapped-out copy (best-effort; on Windows
// the running process still maps it, and the leftover is removed by the
// next upgrade).
func replaceBinary(dst, src string) (func(), error) {
	stage := dst + ".new"
	old := dst + ".old"
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(stage, data, 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(old) // stale leftover from a previous upgrade
	if err := os.Rename(dst, old); err != nil {
		return nil, err
	}
	if err := os.Rename(stage, dst); err != nil {
		// Roll the old image back so the installation isn't left broken.
		_ = os.Rename(old, dst)
		return nil, err
	}
	return func() { _ = os.Remove(old) }, nil
}
