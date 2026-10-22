package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"wiser-dashboard/internal/releasekeys"
)

// Updates come from this project's GitHub releases. Each release's checksum
// list is signed (Ed25519, internal/releasekeys) and the signature must
// verify before the checksums are trusted; the download must match them, and
// the new program must start and report the expected version before it
// replaces the running one.

const updateEvery = 6 * time.Hour

// releaseKeys are the keys release signatures must match (tests swap them).
var releaseKeys = releasekeys.Keys

// releaseBase is where releases are found; WISER_UPDATE_BASE overrides it
// (for tests and mirrors).
var releaseBase = "https://github.com/shenhab/wiser-dashboard/releases"

func init() {
	if b := os.Getenv("WISER_UPDATE_BASE"); b != "" {
		releaseBase = strings.TrimRight(b, "/")
	}
}

// How this install can update itself.
const (
	updateSelf   = "self"   // it can replace its own program and restart
	updateHelper = "helper" // a root helper (systemd) does it for the locked-down service
	updateManual = "manual" // run the installer again
	updateDev    = "dev"    // a development build: no version to compare
)

// helperUnit is installed by install.sh next to the service.
const helperUnit = "/etc/systemd/system/wiser-dashboard-update.path"

// UpdateState is what /api/update reports.
type UpdateState struct {
	Current    string    `json:"current"`
	Latest     string    `json:"latest,omitempty"`
	Available  bool      `json:"available"`
	CheckedAt  time.Time `json:"checkedAt,omitempty"`
	Method     string    `json:"method"`
	Auto       bool      `json:"auto"`
	Busy       string    `json:"busy,omitempty"` // checking, downloading, installing, restarting
	Error      string    `json:"error,omitempty"`
	LastResult string    `json:"lastResult,omitempty"` // how the last update went
	NotesURL   string    `json:"notesUrl,omitempty"`
}

type Updater struct {
	exe      string // this program
	stateDir string // config folder: the helper's request and result files
	client   *http.Client
	restart  func() // asks main to stop and start the (replaced) program again
	auto     func() bool

	mu sync.Mutex
	st UpdateState
}

func NewUpdater(stateDir string, restart func(), auto func() bool) *Updater {
	exe, err := os.Executable()
	if err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
	}
	u := &Updater{exe: exe, stateDir: stateDir, restart: restart, auto: auto,
		client: &http.Client{Timeout: 5 * time.Minute}}
	u.st = UpdateState{Current: version, Method: u.method()}
	u.st.LastResult = u.readHelperResult()
	return u
}

func (u *Updater) State() UpdateState {
	if u == nil {
		return UpdateState{Current: version, Method: updateDev}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.st
	st.Auto = u.auto()
	return st
}

func (u *Updater) set(f func(*UpdateState)) {
	u.mu.Lock()
	f(&u.st)
	u.mu.Unlock()
}

// method works out how this install can be updated.
func (u *Updater) method() string {
	if _, ok := parseVersion(version); !ok || u.exe == "" {
		return updateDev
	}
	if dirWritable(filepath.Dir(u.exe)) {
		return updateSelf
	}
	if _, err := os.Stat(helperUnit); err == nil && dirWritable(u.stateDir) {
		return updateHelper
	}
	return updateManual
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".wiser-dashboard-write-test-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// Run checks for updates now and then, installing them when the user opted in.
func (u *Updater) Run(ctx context.Context) {
	if u.State().Method == updateDev {
		return
	}
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = updateEvery
		if err := u.Check(ctx); err != nil {
			log.Printf("update check: %v", err)
			continue
		}
		if st := u.State(); st.Available && st.Auto && (st.Method == updateSelf || st.Method == updateHelper) {
			log.Printf("installing %s automatically", st.Latest)
			if err := u.Install(ctx); err != nil {
				log.Printf("automatic update: %v", err)
			}
		}
	}
}

// Check looks up the latest release.
func (u *Updater) Check(ctx context.Context) error {
	if u.State().Method == updateDev {
		return errors.New("this is a development build; updates compare release versions")
	}
	u.set(func(s *UpdateState) { s.Busy = "checking" })
	latest, err := latestRelease(ctx, u.client)
	u.set(func(s *UpdateState) {
		s.Busy = ""
		s.CheckedAt = time.Now()
		s.Error = ""
		if err != nil {
			s.Error = err.Error()
			return
		}
		s.Latest = latest
		s.Available = newerVersion(latest, version)
		s.NotesURL = releaseBase + "/tag/" + latest
	})
	return err
}

// Install fetches and installs the latest release, then restarts into it.
func (u *Updater) Install(ctx context.Context) error {
	st := u.State()
	if st.Busy != "" {
		return fmt.Errorf("already %s", st.Busy)
	}
	if !st.Available {
		return errors.New("no newer version to install")
	}
	fail := func(err error) error {
		u.set(func(s *UpdateState) { s.Busy, s.Error = "", err.Error() })
		return err
	}
	switch st.Method {
	case updateSelf:
		u.set(func(s *UpdateState) { s.Busy, s.Error = "downloading", "" })
		if err := installRelease(ctx, u.client, st.Latest, u.exe); err != nil {
			return fail(err)
		}
		log.Printf("installed %s; restarting", st.Latest)
		u.set(func(s *UpdateState) { s.Busy = "restarting" })
		u.restart()
		return nil
	case updateHelper:
		// The root helper (wiser-dashboard-update.path) sees this file, installs
		// the release and restarts the service.
		u.set(func(s *UpdateState) { s.Busy, s.Error = "installing", "" })
		if err := os.WriteFile(filepath.Join(u.stateDir, "update-requested"), []byte(st.Latest+"\n"), 0o644); err != nil {
			return fail(fmt.Errorf("asking the update helper: %w", err))
		}
		go func() {
			// The helper restarts this service; if nothing happens, say so.
			time.Sleep(3 * time.Minute)
			u.set(func(s *UpdateState) {
				if s.Busy == "installing" {
					s.Busy, s.Error = "", "the update helper didn't respond; see: journalctl -u wiser-dashboard-update"
				}
			})
		}()
		return nil
	default:
		return fail(errors.New("this install can't update itself; run the install command again"))
	}
}

func (u *Updater) readHelperResult() string {
	b, err := os.ReadFile(filepath.Join(u.stateDir, "update-result.json"))
	if err != nil {
		return ""
	}
	var r struct {
		Version, Error string
		Time           time.Time
	}
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	if r.Error != "" {
		return fmt.Sprintf("Updating to %s failed %s: %s", r.Version, r.Time.Format("2 Jan 15:04"), r.Error)
	}
	return fmt.Sprintf("Updated to %s on %s", r.Version, r.Time.Format("2 Jan 15:04"))
}

// ---------------------------------------------------------------- versions

type semver struct {
	major, minor, patch int
	pre                 string // "rc1" in v1.4.0-rc1: before the release
	ahead               bool   // a build after the tag (git describe: v1.3.2-3-gabc123)
}

var (
	versionRe  = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-(.+))?$`)
	describeRe = regexp.MustCompile(`^\d+-g[0-9a-f]+(-dirty)?$|^dirty$`)
)

func parseVersion(s string) (semver, bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return semver{}, false
	}
	v := semver{}
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		if describeRe.MatchString(m[4]) {
			v.ahead = true
		} else {
			v.pre = m[4]
		}
	}
	return v, true
}

// newerVersion reports whether release a is newer than the running build b.
func newerVersion(a, b string) bool {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for _, d := range [][2]int{{va.major, vb.major}, {va.minor, vb.minor}, {va.patch, vb.patch}} {
		if d[0] != d[1] {
			return d[0] > d[1]
		}
	}
	// Same numbers: a release beats its own pre-release; a build past the tag
	// is already newer than it.
	if vb.ahead {
		return false
	}
	return va.pre == "" && vb.pre != ""
}

// latestRelease reads the newest tag from the releases/latest redirect.
func latestRelease(ctx context.Context, c *http.Client) (string, error) {
	nc := *c
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, "GET", releaseBase+"/latest", nil)
	if err != nil {
		return "", err
	}
	res, err := nc.Do(req)
	if err != nil {
		return "", fmt.Errorf("can't reach GitHub: %w", err)
	}
	res.Body.Close()
	_, tag, ok := strings.Cut(res.Header.Get("Location"), "/tag/")
	if !ok || tag == "" {
		return "", fmt.Errorf("unexpected answer from GitHub (HTTP %d)", res.StatusCode)
	}
	return tag, nil
}

// releaseAsset names this platform's archive and the program inside it.
func releaseAsset(ver, goos, goarch string) (archive, member string) {
	plat := goos + "-" + goarch
	if goarch == "arm" {
		plat = goos + "-armv7"
	}
	name := "wiser-dashboard-" + ver + "-" + plat
	if goos == "windows" {
		return name + ".zip", name + "/wiser-dashboard.exe"
	}
	return name + ".tar.gz", name + "/wiser-dashboard"
}

// installRelease downloads, verifies and swaps in release ver for exe.
func installRelease(ctx context.Context, c *http.Client, ver, exe string) error {
	archive, member := releaseAsset(ver, runtime.GOOS, runtime.GOARCH)
	sums, err := fetch(ctx, c, releaseBase+"/download/"+ver+"/SHA256SUMS.txt")
	if err != nil {
		return fmt.Errorf("downloading the checksums: %w", err)
	}
	sig, err := fetch(ctx, c, releaseBase+"/download/"+ver+"/SHA256SUMS.txt.sig")
	if err != nil {
		return fmt.Errorf("release %s isn't signed (%v); not installing it", ver, err)
	}
	if err := releasekeys.Verify(sums, string(sig), releaseKeys); err != nil {
		return fmt.Errorf("%w; not installing %s", err, ver)
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == archive {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("%s isn't in the release's checksums", archive)
	}
	data, err := fetch(ctx, c, releaseBase+"/download/"+ver+"/"+archive)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", archive, err)
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return errors.New("checksum mismatch: the download is damaged or was tampered with")
	}
	prog, err := extract(data, archive, member)
	if err != nil {
		return err
	}

	// Next to the program, so the final rename stays on one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".wiser-dashboard-new-*")
	if err != nil {
		return fmt.Errorf("can't write next to %s: %w", exe, err)
	}
	newPath := tmp.Name()
	defer os.Remove(newPath) // gone already after a successful swap
	if _, err := tmp.Write(prog); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if err := os.Chmod(newPath, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := os.Rename(newPath, newPath+".exe"); err != nil {
			return err
		}
		newPath += ".exe"
		defer os.Remove(newPath)
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, newPath, "-version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "wiser-dashboard "+ver {
		return fmt.Errorf("the new program didn't start correctly (%v: %q)", err, strings.TrimSpace(string(out)))
	}
	return swapProgram(newPath, exe)
}

// swapProgram puts newPath in place of exe. Windows can't overwrite a running
// program but can rename it, so the old one moves aside first.
func swapProgram(newPath, exe string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(newPath, exe)
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	return nil
}

func fetch(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 512<<20))
}

// extract returns one file from a .tar.gz or .zip archive.
func extract(data []byte, archive, member string) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Name == member {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("%s isn't in %s", member, archive)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s isn't in %s", member, archive)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == member {
			return io.ReadAll(tr)
		}
	}
}

// ---------------------------------------------------------------- the root helper

// runSelfUpdate is "wiser-dashboard self-update", run as root by the systemd
// helper (wiser-dashboard-update.service) when the locked-down service asks
// for an update: install the latest release over this program, restart the
// service, and leave the outcome for the dashboard to show.
func runSelfUpdate(args []string) {
	fs := flag.NewFlagSet("self-update", flag.ExitOnError)
	stateDir := fs.String("state", "/var/lib/wiser-dashboard", "the service's state folder")
	service := fs.String("service", "wiser-dashboard", "the systemd service to restart")
	fs.Parse(args)

	os.Remove(filepath.Join(*stateDir, "update-requested")) // or the .path unit fires again
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := &http.Client{Timeout: 5 * time.Minute}
	latest := ""
	if err == nil {
		latest, err = latestRelease(ctx, c)
	}
	if err == nil && !newerVersion(latest, version) {
		err = fmt.Errorf("already up to date (%s)", version)
	}
	if err == nil {
		err = installRelease(ctx, c, latest, exe)
	}
	result := map[string]any{"version": latest, "time": time.Now()}
	if err != nil {
		result["error"] = err.Error()
	}
	b, _ := json.Marshal(result)
	os.WriteFile(filepath.Join(*stateDir, "update-result.json"), b, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "self-update:", err)
		os.Exit(1)
	}
	fmt.Printf("installed %s; restarting %s\n", latest, *service)
	if out, err := exec.Command("systemctl", "restart", *service).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "restarting %s: %v %s\n", *service, err, out)
		os.Exit(1)
	}
}
