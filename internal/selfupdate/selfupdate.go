// Package selfupdate replaces the running executable with the latest GitHub
// release of the same major version. Every download is checked against
// checksums.txt, whose minisign signature must come from one of the built-in
// release keys; the new executable must report the expected version before it
// replaces the old one.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	Name        = "jwlSyncFolderMerger"
	DefaultRepo = "chaosben/jwlSyncFolderMerger"
	DefaultAPI  = "https://api.github.com"

	// MinInterval keeps watch mode well below GitHub's limit of 60
	// unauthenticated API requests per hour.
	MinInterval = time.Hour

	checksumsName = "checksums.txt"
	signatureName = checksumsName + ".minisig"
	maxArchive    = 100 << 20
	maxBinary     = 200 << 20
)

// ValidateInterval rejects update intervals below MinInterval.
func ValidateInterval(d time.Duration) error {
	if d < MinInterval {
		return fmt.Errorf("update interval %s is too short: it must be at least %s", d, MinInterval)
	}
	return nil
}

type Updater struct {
	Version string // version of the running executable, e.g. "0.1.0"
	Repo    string // GitHub owner/name
	APIBase string
	Keys    []string // trusted minisign public keys
	Client  *http.Client
	Exe     string // executable to replace
	GOOS    string
	GOARCH  string
	GOARM   string // "6" or "7" for GOARCH arm
	Log     *slog.Logger
	// Probe returns the output of "<exe> -version"; nil runs the executable.
	Probe func(ctx context.Context, exe string) (string, error)
}

// New returns an updater for the running executable.
func New(version string, log *slog.Logger) (*Updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate executable: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, fmt.Errorf("locate executable: %w", err)
	}
	return &Updater{
		Version: version,
		Repo:    DefaultRepo,
		APIBase: DefaultAPI,
		Keys:    ReleaseKeys,
		Client:  &http.Client{Timeout: 10 * time.Minute},
		Exe:     exe,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		GOARM:   buildGOARM(),
		Log:     log,
	}, nil
}

// Supported returns why this installation cannot update itself, or nil.
func (u *Updater) Supported() error {
	if _, err := parseVersion(u.Version); err != nil {
		return fmt.Errorf("version %q is not a release build", u.Version)
	}
	dir := filepath.Dir(u.Exe)
	f, err := os.CreateTemp(dir, "."+Name+".probe-*")
	if err != nil {
		return fmt.Errorf("install directory %s is not writable", dir)
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

// Release is a newer release that can be installed.
type Release struct {
	Version string
	assets  map[string]string // name -> download URL
}

// Check returns the latest release if it is newer than the running version and
// has the same major version; nil means there is nothing to install.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	cur, err := parseVersion(u.Version)
	if err != nil {
		return nil, fmt.Errorf("version %q is not a release build", u.Version)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.APIBase+"/repos/"+u.Repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", Name+"/"+u.Version)
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query latest release: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil // no release published yet
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("query latest release: %s", resp.Status)
	}
	var latest struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&latest); err != nil {
		return nil, fmt.Errorf("query latest release: %w", err)
	}
	if latest.Draft || latest.Prerelease {
		return nil, nil
	}
	v, err := parseVersion(latest.TagName)
	if err != nil {
		return nil, nil // e.g. a pre-release tag that is not marked as such
	}
	if !cur.less(v) {
		u.Log.Debug("no update available", "version", u.Version, "latest", v.String())
		return nil, nil
	}
	if v[0] != cur[0] {
		u.Log.Info("new major version available, please update manually", "version", u.Version, "latest", v.String())
		return nil, nil
	}
	rel := &Release{Version: v.String(), assets: map[string]string{}}
	for _, a := range latest.Assets {
		rel.assets[a.Name] = a.URL
	}
	return rel, nil
}

// Update installs the latest release if there is one and reports whether the
// executable was replaced.
func (u *Updater) Update(ctx context.Context) (bool, error) {
	rel, err := u.Check(ctx)
	if err != nil || rel == nil {
		return false, err
	}
	u.Log.Info("update available", "version", u.Version, "new_version", rel.Version)
	if err := u.Apply(ctx, rel); err != nil {
		return false, fmt.Errorf("update to %s: %w", rel.Version, err)
	}
	u.Log.Info("updated", "from", u.Version, "to", rel.Version, "executable", u.Exe)
	return true, nil
}

// Apply downloads, verifies and installs rel. The running executable is only
// touched after every check has passed.
func (u *Updater) Apply(ctx context.Context, rel *Release) error {
	asset := u.assetName(rel.Version)
	for _, name := range []string{asset, checksumsName, signatureName} {
		if rel.assets[name] == "" {
			return fmt.Errorf("release has no %s", name)
		}
	}
	sums, err := u.fetch(ctx, rel.assets[checksumsName], 1<<20)
	if err != nil {
		return err
	}
	sig, err := u.fetch(ctx, rel.assets[signatureName], 1<<14)
	if err != nil {
		return err
	}
	if _, err := verifyMinisign(u.Keys, sums, sig); err != nil {
		return fmt.Errorf("%s: %w", checksumsName, err)
	}
	want, err := checksumOf(sums, asset)
	if err != nil {
		return err
	}
	archive, err := u.fetch(ctx, rel.assets[asset], maxArchive)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s: checksum mismatch", asset)
	}
	bin, err := extract(archive, asset, u.binaryName())
	if err != nil {
		return fmt.Errorf("%s: %w", asset, err)
	}

	dir := filepath.Dir(u.Exe)
	tmp, err := os.CreateTemp(dir, "."+Name+".new-*"+filepath.Ext(u.binaryName()))
	if err != nil {
		return fmt.Errorf("write new executable: %w", err)
	}
	installed := false
	defer func() {
		if !installed {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return fmt.Errorf("write new executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write new executable: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return fmt.Errorf("write new executable: %w", err)
	}

	out, err := u.probe(ctx, tmp.Name())
	if err != nil {
		return fmt.Errorf("new executable does not run: %w", err)
	}
	if want := Name + " " + rel.Version; strings.TrimSpace(out) != want {
		return fmt.Errorf("new executable reports %q instead of %q", strings.TrimSpace(out), want)
	}
	if err := replace(tmp.Name(), u.Exe); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	installed = true
	return nil
}

// CleanupOld removes executables left over by earlier updates. Files that are
// still in use (Windows) are kept for a later attempt.
func (u *Updater) CleanupOld() {
	old, _ := filepath.Glob(u.Exe + ".old-*")
	for _, p := range old {
		if os.Remove(p) == nil {
			u.Log.Debug("removed old executable", "file", p)
		}
	}
	tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(u.Exe), "."+Name+".new-*"))
	for _, p := range tmp {
		// a concurrent update of another instance may still be writing it
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Hour {
			os.Remove(p)
		}
	}
}

// replace moves the new executable into place. Windows cannot overwrite a
// running executable but can rename it.
func replace(newExe, exe string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(newExe, exe)
	}
	old := fmt.Sprintf("%s.old-%d", exe, time.Now().UnixNano())
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(newExe, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("%w (restoring the old executable failed: %v)", err, rerr)
		}
		return err
	}
	return nil
}

func (u *Updater) probe(ctx context.Context, exe string) (string, error) {
	if u.Probe != nil {
		return u.Probe(ctx, exe)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "-version").Output()
	return string(out), err
}

func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", Name+"/"+u.Version)
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", path.Base(url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", path.Base(url), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", path.Base(url), err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", path.Base(url), limit)
	}
	return b, nil
}

// assetName follows the archive name_template in .goreleaser.yaml.
func (u *Updater) assetName(version string) string {
	arch := u.GOARCH
	if arch == "arm" {
		arch += "v" + u.GOARM
	}
	ext := ".tar.gz"
	if u.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s%s", Name, version, u.GOOS, arch, ext)
}

func (u *Updater) binaryName() string {
	if u.GOOS == "windows" {
		return Name + ".exe"
	}
	return Name
}

// buildGOARM returns the ARM version the executable was built for.
func buildGOARM() string {
	if runtime.GOARCH != "arm" {
		return ""
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" && s.Value != "" && s.Value[0] >= '5' && s.Value[0] <= '7' {
				return s.Value[:1]
			}
		}
	}
	return "6" // runs on every Raspberry Pi
}

// checksumOf finds the SHA-256 of name in a checksums.txt ("<hex>  <name>").
func checksumOf(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s lists no checksum for %s", checksumsName, name)
}

// extract returns the executable from a release archive; it is expected at
// the top level, as GoReleaser puts it.
func extract(archive []byte, archiveName, binary string) ([]byte, error) {
	read := func(r io.Reader) ([]byte, error) {
		b, err := io.ReadAll(io.LimitReader(r, maxBinary+1))
		if err == nil && len(b) > maxBinary {
			err = errors.New("executable too large")
		}
		return b, err
	}
	if strings.HasSuffix(archiveName, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Clean(f.Name) == binary && f.Mode().IsRegular() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return read(rc)
			}
		}
		return nil, fmt.Errorf("%s not found in archive", binary)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in archive", binary)
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(h.Name) == binary && h.Typeflag == tar.TypeReg {
			return read(tr)
		}
	}
}

// version is a release version major.minor.patch; anything else (dev builds,
// snapshots, pre-releases) is not updated.
type version [3]int

func parseVersion(s string) (version, error) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	var v version
	if len(parts) != 3 {
		return v, fmt.Errorf("not a release version: %q", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || p[0] == '+' {
			return v, fmt.Errorf("not a release version: %q", s)
		}
		v[i] = n
	}
	return v, nil
}

func (v version) less(o version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }
