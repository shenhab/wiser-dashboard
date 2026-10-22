package main

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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wiser-dashboard/internal/releasekeys"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		release, running string
		want             bool
	}{
		{"v1.3.2", "v1.3.1", true},
		{"v1.4.0", "v1.3.9", true},
		{"v2.0.0", "v1.10.3", true},
		{"v1.10.0", "v1.9.0", true}, // numbers, not text
		{"v1.3.1", "v1.3.1", false},
		{"v1.3.1", "v1.3.2", false},
		{"v1.4.0", "v1.4.0-rc1", true},         // the release after its release candidate
		{"v1.4.0-rc2", "v1.4.0", false},        // never "update" back to a pre-release
		{"v1.3.2", "v1.3.2-4-gabc1234", false}, // a build past the tag is already newer
		{"v1.3.3", "v1.3.2-4-gabc1234-dirty", true},
		{"v1.3.2", "dev", false},
		{"nonsense", "v1.0.0", false},
	} {
		if got := newerVersion(c.release, c.running); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.release, c.running, got, c.want)
		}
	}
}

func TestReleaseAsset(t *testing.T) {
	for _, c := range []struct{ goos, goarch, archive, member string }{
		{"linux", "amd64", "wiser-dashboard-v1.2.3-linux-amd64.tar.gz", "wiser-dashboard-v1.2.3-linux-amd64/wiser-dashboard"},
		{"linux", "arm", "wiser-dashboard-v1.2.3-linux-armv7.tar.gz", "wiser-dashboard-v1.2.3-linux-armv7/wiser-dashboard"},
		{"windows", "arm64", "wiser-dashboard-v1.2.3-windows-arm64.zip", "wiser-dashboard-v1.2.3-windows-arm64/wiser-dashboard.exe"},
	} {
		a, m := releaseAsset("v1.2.3", c.goos, c.goarch)
		if a != c.archive || m != c.member {
			t.Errorf("%s/%s: %s %s", c.goos, c.goarch, a, m)
		}
	}
}

// How fakeRelease signs its checksum list.
const (
	signed        = "signed"
	unsigned      = "unsigned"       // no .sig file
	foreignKey    = "foreign key"    // signed, but not with a release key
	tamperedSums  = "tampered sums"  // signed, then the checksums changed
	corruptBinary = "corrupt binary" // properly signed, but the download is damaged
)

// fakeRelease serves a signed release of a stand-in program that prints
// "prints" for -version, the way GitHub serves releases; it makes the test's
// own key the only accepted release key.
func fakeRelease(t *testing.T, tag, prints, mode string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	os.WriteFile(src, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\""+prints+"\") }\n"), 0o644)
	bin := filepath.Join(dir, "prog")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("building the stand-in: %v %s", err, out)
	}
	prog, _ := os.ReadFile(bin)
	archive, member := releaseAsset(tag, runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	if strings.HasSuffix(archive, ".zip") {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(member)
		w.Write(prog)
		zw.Close()
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(prog))})
		tw.Write(prog)
		tw.Close()
		gz.Close()
	}
	sum := sha256.Sum256(buf.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + archive + "\n"
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := priv
	if mode == foreignKey {
		_, signer, _ = ed25519.GenerateKey(rand.Reader)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(signer, []byte(sums)))
	if mode == tamperedSums {
		sums += strings.Repeat("0", 64) + "  extra-file.tar.gz\n"
	}
	if mode == corruptBinary {
		buf.WriteString("garbage")
	}
	oldKeys := releaseKeys
	releaseKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	t.Cleanup(func() { releaseKeys = oldKeys })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
		case "/releases/download/" + tag + "/SHA256SUMS.txt":
			w.Write([]byte(sums))
		case "/releases/download/" + tag + "/SHA256SUMS.txt.sig":
			if mode == unsigned {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(sig + "\n"))
		case "/releases/download/" + tag + "/" + archive:
			w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releaseBase
	releaseBase = srv.URL + "/releases"
	t.Cleanup(func() { releaseBase = old })
}

// installedProgram is a stand-in for the running program, to be replaced.
func installedProgram(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "wiser-dashboard")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	os.WriteFile(exe, []byte("old program"), 0o755)
	return exe
}

func TestInstallRelease(t *testing.T) {
	ctx := context.Background()
	c := http.DefaultClient

	fakeRelease(t, "v9.9.9", "wiser-dashboard v9.9.9", signed)
	if tag, err := latestRelease(ctx, c); err != nil || tag != "v9.9.9" {
		t.Fatalf("latest: %q %v", tag, err)
	}
	exe := installedProgram(t)
	if err := installRelease(ctx, c, "v9.9.9", exe); err != nil {
		t.Fatalf("install: %v", err)
	}
	if out, err := exec.Command(exe, "-version").Output(); err != nil || strings.TrimSpace(string(out)) != "wiser-dashboard v9.9.9" {
		t.Errorf("after the update the program says %q (%v)", out, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".wiser-dashboard-new-*")); len(left) != 0 {
		t.Errorf("leftover files: %v", left)
	}

	// Refused, and the old program stays: a damaged download, and releases
	// whose checksums aren't signed by a release key.
	for _, c2 := range []struct{ mode, want string }{
		{corruptBinary, "checksum mismatch"},
		{unsigned, "isn't signed"},
		{foreignKey, "doesn't match this project's signing keys"},
		{tamperedSums, "doesn't match this project's signing keys"},
	} {
		fakeRelease(t, "v9.9.9", "wiser-dashboard v9.9.9", c2.mode)
		exe = installedProgram(t)
		if err := installRelease(ctx, c, "v9.9.9", exe); err == nil || !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s: got %v, want an error about %q", c2.mode, err, c2.want)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old program" {
			t.Errorf("%s: the program was replaced", c2.mode)
		}
	}

	// A program that doesn't report the expected version is refused.
	fakeRelease(t, "v9.9.9", "something else", signed)
	exe = installedProgram(t)
	if err := installRelease(ctx, c, "v9.9.9", exe); err == nil || !strings.Contains(err.Error(), "didn't start correctly") {
		t.Errorf("wrong program: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old program" {
		t.Error("a wrong program replaced the old one")
	}
}

func TestReleaseKeys(t *testing.T) {
	if len(releasekeys.Keys) < 2 {
		t.Fatal("want a main and a spare release key")
	}
	for _, k := range releasekeys.Keys {
		if b, err := base64.StdEncoding.DecodeString(k); err != nil || len(b) != ed25519.PublicKeySize {
			t.Errorf("release key %q isn't an Ed25519 public key", k)
		}
	}
}
