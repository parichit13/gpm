package cmd

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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/parichit13/gpm/internal/ipc"
)

// Version is the build version, set via -ldflags
// "-X github.com/parichit13/gpm/cmd.Version=v1.2.3". "dev" for local builds.
var Version = "dev"

// Release source. Public binaries are published to GitHub Releases of this repo.
const (
	repoOwner = "parichit13"
	repoName  = "gpm"
)

// assetName is the release asset for this platform, matching the names produced
// by .goreleaser.yaml (raw binaries: gpm_<os>_<arch>).
func assetName() string {
	return fmt.Sprintf("gpm_%s_%s", runtime.GOOS, runtime.GOARCH)
}

var (
	flagUpdateCheck bool
	flagUpdateForce bool
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the gpm version",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Printf("gpm %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return nil
	},
}

var updateCmd = &cobra.Command{
	Use:     "update",
	Aliases: []string{"upgrade", "self-update"},
	Short:   "Update gpm to the latest published release",
	Long: "Check GitHub Releases for a newer gpm, download the binary for this\n" +
		"platform, verify its checksum, and replace the installed binary in place.\n" +
		"Use --check to only report whether an update is available.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUpdate()
	},
}

// ─── release metadata ───────────────────────────────────────────────────────

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

func latestRelease() (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no published releases found for %s/%s yet", repoOwner, repoName)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// ─── update flow ────────────────────────────────────────────────────────────

func runUpdate() error {
	fmt.Printf("Current version: %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
	fmt.Println("Checking for updates…")

	rel, err := latestRelease()
	if err != nil {
		return err
	}
	latest := rel.TagName
	fmt.Printf("Latest version:  %s\n", latest)

	cmp := compareVersions(Version, latest)
	if cmp >= 0 && !flagUpdateForce {
		fmt.Println("gpm is already up to date.")
		return nil
	}
	if flagUpdateCheck {
		fmt.Printf("An update is available: %s → %s\n", Version, latest)
		fmt.Println("Run `gpm update` to install it.")
		return nil
	}

	// Find the asset + checksums for this platform.
	want := assetName()
	var assetURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			assetURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		return fmt.Errorf("release %s has no binary for %s/%s (asset %q); see %s",
			latest, runtime.GOOS, runtime.GOARCH, want, rel.HTMLURL)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)

	fmt.Printf("Downloading %s…\n", want)
	data, err := download(assetURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	// Verify checksum when checksums.txt is published.
	if sumsURL != "" {
		sums, err := download(sumsURL)
		if err != nil {
			return fmt.Errorf("cannot fetch checksums: %w", err)
		}
		if err := verifyChecksum(data, want, string(sums)); err != nil {
			return err
		}
		fmt.Println("Checksum verified.")
	} else {
		fmt.Println("Warning: no checksums.txt in release; skipping verification.")
	}

	if err := replaceBinary(exe, data); err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	fmt.Printf("Installed gpm %s to %s\n", latest, exe)

	// Apply to the running daemon, preserving services.
	restartDaemonForUpdate()

	fmt.Printf("\ngpm updated to %s.\n", latest)
	return nil
}

func download(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func verifyChecksum(data []byte, name, sums string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			if !strings.EqualFold(fields[0], got) {
				return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", name, fields[0], got)
			}
			return nil
		}
	}
	return fmt.Errorf("no checksum entry for %s in checksums.txt", name)
}

// replaceBinary writes data to a temp file in the same directory as exe, makes
// it executable, ad-hoc-signs it on macOS, then atomically renames it over exe.
// The rename gives a fresh inode (the running process keeps the old one), which
// avoids the macOS code-signing "Killed: 9" you get from overwriting in place.
func replaceBinary(exe string, data []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".gpm-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (need write access there): %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		// Best-effort ad-hoc re-sign so Gatekeeper/AMFI doesn't kill it.
		_ = exec.Command("codesign", "--force", "--sign", "-", tmpPath).Run()
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("cannot replace %s: %w", exe, err)
	}
	return nil
}

// restartDaemonForUpdate restarts the daemon so it runs the new binary, saving
// and resurrecting services around it. Managed services restart briefly — that
// is inherent to swapping the manager itself.
func restartDaemonForUpdate() {
	if !ipc.PingRaw() {
		return // not running; next start uses the new binary
	}
	fmt.Println("Restarting the gpm daemon onto the new version (services will briefly restart)…")
	_, _ = ipc.Send(ipc.Request{Action: ipc.ActionSave})

	if err := stopDaemon(); err != nil {
		fmt.Printf("  could not stop daemon automatically: %v\n  run: gpm daemon stop && gpm daemon start && gpm resurrect\n", err)
		return
	}
	// Wait for it to exit.
	for i := 0; i < 25 && ipc.PingRaw(); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := startDaemonProcess(); err != nil {
		fmt.Printf("  daemon restart failed: %v\n", err)
		return
	}
	if _, err := ipc.Send(ipc.Request{Action: ipc.ActionResurrect}); err != nil {
		fmt.Printf("  resurrect failed: %v (run: gpm resurrect)\n", err)
		return
	}
	fmt.Println("  daemon restarted; services resurrected.")
}

// compareVersions returns -1, 0, or 1 comparing semver-ish tags (a vs b). A
// "dev" current version always sorts below a real release.
func compareVersions(a, b string) int {
	if a == b {
		return 0
	}
	if a == "dev" || a == "" {
		return -1
	}
	an := splitVersion(a)
	bn := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 { // drop prerelease/build metadata
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}
