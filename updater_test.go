package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

// fakeRelease serves a release of a stand-in program that prints "prints"
// for -version, the way GitHub serves releases.
func fakeRelease(t *testing.T, tag, prints string, corrupt bool) {
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
	if corrupt {
		buf.WriteString("garbage")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
		case "/releases/download/" + tag + "/SHA256SUMS.txt":
			w.Write([]byte(sums))
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

	fakeRelease(t, "v9.9.9", "wiser-dashboard v9.9.9", false)
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

	// A damaged download is refused and the old program stays.
	fakeRelease(t, "v9.9.9", "wiser-dashboard v9.9.9", true)
	exe = installedProgram(t)
	if err := installRelease(ctx, c, "v9.9.9", exe); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("damaged download: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old program" {
		t.Error("a damaged download replaced the program")
	}

	// A program that doesn't report the expected version is refused.
	fakeRelease(t, "v9.9.9", "something else", false)
	exe = installedProgram(t)
	if err := installRelease(ctx, c, "v9.9.9", exe); err == nil || !strings.Contains(err.Error(), "didn't start correctly") {
		t.Errorf("wrong program: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old program" {
		t.Error("a wrong program replaced the old one")
	}
}
