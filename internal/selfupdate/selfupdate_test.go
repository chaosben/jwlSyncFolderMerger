package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"
)

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixtures in testdata were made with minisign 0.12 and a throwaway key.
func TestVerifyMinisignFixtures(t *testing.T) {
	pub := string(readFile(t, "testdata/test.pub"))
	msg := readFile(t, "testdata/msg.txt")
	tests := []struct {
		name    string
		keys    []string
		msg     []byte
		sig     string
		trusted string
		wantErr string
	}{
		{"prehashed", []string{pub}, msg, "prehashed.minisig", "jwlSyncFolderMerger 0.2.0", ""},
		{"legacy", []string{pub}, msg, "legacy.minisig", "legacy", ""},
		{"second key matches", append([]string{ReleaseKeys[0]}, pub), msg, "prehashed.minisig", "jwlSyncFolderMerger 0.2.0", ""},
		{"tampered message", []string{pub}, append([]byte("x"), msg...), "prehashed.minisig", "", "verification failed"},
		{"unknown key", ReleaseKeys, msg, "prehashed.minisig", "", "unknown key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := readFile(t, filepath.Join("testdata", tt.sig))
			trusted, err := verifyMinisign(tt.keys, tt.msg, sig)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || trusted != tt.trusted {
				t.Fatalf("trusted = %q, err = %v", trusted, err)
			}
		})
	}
}

func TestVerifyMinisignTamperedTrustedComment(t *testing.T) {
	pub := string(readFile(t, "testdata/test.pub"))
	sig := strings.Replace(string(readFile(t, "testdata/prehashed.minisig")), "0.2.0", "9.9.9", 1)
	if _, err := verifyMinisign([]string{pub}, readFile(t, "testdata/msg.txt"), []byte(sig)); err == nil ||
		!strings.Contains(err.Error(), "trusted comment") {
		t.Fatalf("err = %v", err)
	}
}

func TestReleaseKeyMatchesFile(t *testing.T) {
	file, err := parsePublicKey(string(readFile(t, "../../keys/release_sign.pub")))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ReleaseKeys {
		pk, err := parsePublicKey(k)
		if err != nil {
			t.Fatal(err)
		}
		if pk.id == file.id && pk.key.Equal(file.key) {
			return
		}
	}
	t.Fatal("keys/release_sign.pub is not among ReleaseKeys")
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" = invalid
	}{
		{"0.1.0", "0.1.0"},
		{"v1.2.3", "1.2.3"},
		{"dev", ""},
		{"1.2", ""},
		{"1.2.3-rc1", ""},
		{"0.1.0-SNAPSHOT-abc", ""},
		{"1.+2.3", ""},
		{"1..3", ""},
	}
	for _, tt := range tests {
		v, err := parseVersion(tt.in)
		if got := map[bool]string{true: v.String(), false: ""}[err == nil]; got != tt.want {
			t.Errorf("parseVersion(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	a, _ := parseVersion("0.9.0")
	b, _ := parseVersion("0.10.0")
	if !a.less(b) || b.less(a) || a.less(a) {
		t.Error("0.9.0 must be less than 0.10.0")
	}
}

func TestValidateInterval(t *testing.T) {
	tests := []struct {
		d  time.Duration
		ok bool
	}{
		{0, false},
		{59 * time.Minute, false},
		{time.Hour, true},
		{24 * time.Hour, true},
	}
	for _, tt := range tests {
		if err := ValidateInterval(tt.d); (err == nil) != tt.ok {
			t.Errorf("ValidateInterval(%s) = %v", tt.d, err)
		}
	}
}

func TestAssetName(t *testing.T) {
	tests := []struct{ goos, goarch, goarm, want string }{
		{"linux", "amd64", "", "jwlSyncFolderMerger_0.2.0_linux_amd64.tar.gz"},
		{"linux", "arm", "7", "jwlSyncFolderMerger_0.2.0_linux_armv7.tar.gz"},
		{"linux", "arm", "6", "jwlSyncFolderMerger_0.2.0_linux_armv6.tar.gz"},
		{"darwin", "arm64", "", "jwlSyncFolderMerger_0.2.0_darwin_arm64.tar.gz"},
		{"windows", "amd64", "", "jwlSyncFolderMerger_0.2.0_windows_amd64.zip"},
	}
	for _, tt := range tests {
		u := &Updater{GOOS: tt.goos, GOARCH: tt.goarch, GOARM: tt.goarm}
		if got := u.assetName("0.2.0"); got != tt.want {
			t.Errorf("assetName = %q, want %q", got, tt.want)
		}
	}
}

// signer produces minisign signatures like "minisign -S" (prehashed).
type signer struct {
	id   [8]byte
	priv ed25519.PrivateKey
	pub  string
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &signer{priv: priv}
	rand.Read(s.id[:])
	s.pub = base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), s.id[:]...), pub...))
	return s
}

func (s *signer) sign(msg []byte, trusted string) []byte {
	h := blake2b.Sum512(msg)
	sig := ed25519.Sign(s.priv, h[:])
	global := ed25519.Sign(s.priv, append(append([]byte{}, sig...), trusted...))
	return fmt.Appendf(nil, "untrusted comment: signature from minisign secret key\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(append(append([]byte("ED"), s.id[:]...), sig...)),
		trusted, base64.StdEncoding.EncodeToString(global))
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"README.md", []byte("readme")}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(content)
	zw.Close()
	return buf.Bytes()
}

// fakeRelease is a release as served by the fake GitHub.
type fakeRelease struct {
	tag        string
	prerelease bool
	files      map[string][]byte
}

func makeRelease(t *testing.T, s *signer, ver, goos string) *fakeRelease {
	t.Helper()
	u := &Updater{GOOS: goos, GOARCH: "amd64"}
	asset := u.assetName(ver)
	bin := []byte("new executable " + ver)
	var archive []byte
	if goos == "windows" {
		archive = zipped(t, u.binaryName(), bin)
	} else {
		archive = tarGz(t, u.binaryName(), bin)
	}
	sum := sha256.Sum256(archive)
	sums := []byte(fmt.Sprintf("%s  %s\n%s  other.tar.gz\n", hex.EncodeToString(sum[:]), asset, strings.Repeat("0", 64)))
	return &fakeRelease{tag: "v" + ver, files: map[string][]byte{
		asset:         archive,
		checksumsName: sums,
		signatureName: s.sign(sums, Name+" "+ver),
	}}
}

func serve(t *testing.T, rel *fakeRelease) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/releases/latest":
			if rel == nil {
				http.NotFound(w, r)
				return
			}
			type asset struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			}
			var assets []asset
			for name := range rel.files {
				assets = append(assets, asset{name, srv.URL + "/dl/" + name})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": rel.tag, "prerelease": rel.prerelease, "assets": assets})
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			b, ok := rel.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newUpdater(t *testing.T, srv *httptest.Server, s *signer, version, goos string) *Updater {
	t.Helper()
	exe := filepath.Join(t.TempDir(), Name)
	if goos == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(exe, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Updater{
		Version: version, Repo: "o/r", APIBase: srv.URL, Keys: []string{s.pub},
		Client: srv.Client(), Exe: exe, GOOS: goos, GOARCH: "amd64",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Probe: func(_ context.Context, exe string) (string, error) {
			b, err := os.ReadFile(exe)
			return Name + " " + strings.TrimPrefix(string(b), "new executable ") + "\n", err
		},
	}
}

func TestUpdate(t *testing.T) {
	s := newSigner(t)
	tests := []struct {
		name    string
		version string
		goos    string
		release func(*fakeRelease) *fakeRelease // nil: release 0.2.0 unchanged
		probe   func(context.Context, string) (string, error)
		updated bool
		wantErr string
	}{
		{name: "newer release", version: "0.1.0", updated: true},
		{name: "newer release windows", version: "0.1.0", goos: "windows", updated: true},
		{name: "same version", version: "0.2.0"},
		{name: "running version is newer", version: "0.3.0"},
		{name: "new major version", version: "0.1.0",
			release: func(r *fakeRelease) *fakeRelease { r.tag = "v1.0.0"; return r }},
		{name: "pre-release", version: "0.1.0",
			release: func(r *fakeRelease) *fakeRelease { r.prerelease = true; return r }},
		{name: "unmarked pre-release tag", version: "0.1.0",
			release: func(r *fakeRelease) *fakeRelease { r.tag = "v0.2.0-rc1"; return r }},
		{name: "no release yet", version: "0.1.0",
			release: func(*fakeRelease) *fakeRelease { return nil }},
		{name: "tampered archive", version: "0.1.0", wantErr: "checksum mismatch",
			release: func(r *fakeRelease) *fakeRelease {
				a := (&Updater{GOOS: "linux", GOARCH: "amd64"}).assetName("0.2.0")
				r.files[a] = tarGz(t, Name, []byte("evil"))
				return r
			}},
		{name: "signed with another key", version: "0.1.0", wantErr: "unknown key",
			release: func(r *fakeRelease) *fakeRelease {
				r.files[signatureName] = newSigner(t).sign(r.files[checksumsName], "x")
				return r
			}},
		{name: "checksums changed after signing", version: "0.1.0", wantErr: "verification failed",
			release: func(r *fakeRelease) *fakeRelease {
				r.files[checksumsName] = append(r.files[checksumsName], " "...)
				return r
			}},
		{name: "signature missing", version: "0.1.0", wantErr: "no checksums.txt.minisig",
			release: func(r *fakeRelease) *fakeRelease { delete(r.files, signatureName); return r }},
		{name: "no archive for this platform", version: "0.1.0", goos: "freebsd", wantErr: "release has no"},
		{name: "new executable reports wrong version", version: "0.1.0", wantErr: "instead of",
			probe: func(context.Context, string) (string, error) { return Name + " 0.1.9\n", nil }},
		{name: "new executable does not run", version: "0.1.0", wantErr: "does not run",
			probe: func(context.Context, string) (string, error) { return "", fmt.Errorf("exec format error") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}
			relOS := goos
			if goos == "freebsd" {
				relOS = "linux"
			}
			rel := makeRelease(t, s, "0.2.0", relOS)
			if tt.release != nil {
				rel = tt.release(rel)
			}
			srv := serve(t, rel)
			u := newUpdater(t, srv, s, tt.version, goos)
			if tt.probe != nil {
				u.Probe = tt.probe
			}

			updated, err := u.Update(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if updated != tt.updated {
				t.Fatalf("updated = %v, want %v", updated, tt.updated)
			}
			want := "old executable"
			if tt.updated {
				want = "new executable 0.2.0"
			}
			if got := string(readFile(t, u.Exe)); got != want {
				t.Errorf("executable = %q, want %q", got, want)
			}
			entries, _ := os.ReadDir(filepath.Dir(u.Exe))
			if len(entries) != 1 {
				t.Errorf("leftover files next to the executable: %v", entries)
			}
		})
	}
}

// The default probe really runs the new executable.
func TestApplyRunsNewExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as executable")
	}
	s := newSigner(t)
	rel := makeRelease(t, s, "0.2.0", "linux")
	srv := serve(t, rel)
	u := newUpdater(t, srv, s, "0.1.0", "linux")
	u.Probe = nil
	script := "#!/bin/sh\necho '" + Name + " 0.2.0'\n"
	a := u.assetName("0.2.0")
	rel.files[a] = tarGz(t, Name, []byte(script))
	sum := sha256.Sum256(rel.files[a])
	rel.files[checksumsName] = []byte(hex.EncodeToString(sum[:]) + "  " + a + "\n")
	rel.files[signatureName] = s.sign(rel.files[checksumsName], "t")

	if updated, err := u.Update(context.Background()); err != nil || !updated {
		t.Fatalf("updated = %v, err = %v", updated, err)
	}
	if got := string(readFile(t, u.Exe)); got != script {
		t.Fatalf("executable = %q", got)
	}
}

func TestSupported(t *testing.T) {
	u := &Updater{Version: "dev", Exe: filepath.Join(t.TempDir(), Name)}
	if err := u.Supported(); err == nil || !strings.Contains(err.Error(), "not a release build") {
		t.Fatalf("dev build: err = %v", err)
	}
	u.Version = "0.1.0"
	if err := u.Supported(); err != nil {
		t.Fatalf("writable dir: err = %v", err)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	dir := filepath.Dir(u.Exe)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := u.Supported(); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("read-only dir: err = %v", err)
	}
}

func TestCleanupOld(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, Name)
	keep := filepath.Join(dir, "."+Name+".new-recent")
	for _, p := range []string{exe, exe + ".old-123", filepath.Join(dir, "."+Name+".new-stale"), keep} {
		os.WriteFile(p, nil, 0o755)
	}
	stale := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(dir, "."+Name+".new-stale"), stale, stale)

	(&Updater{Exe: exe, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).CleanupOld()

	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"." + Name + ".new-recent", Name}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("left %v, want %v", names, want)
	}
}
