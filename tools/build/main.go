// Command build builds wiser-dashboard the same way on Windows, macOS and
// Linux, needing only Go. When Node.js is installed it also builds the Matter
// bridge (matter/) and embeds it, as the releases do.
//
//	go run ./tools/build                         # this machine -> ./wiser-dashboard
//	go run ./tools/build -all                    # every platform -> dist/
//	go run ./tools/build -target linux/arm64     # one platform -> dist/
//	go run ./tools/build -all -package           # release archives -> dist/
//	go run ./tools/build -matter=off             # skip the Matter bridge
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// allTargets are the platforms releases are built for.
var allTargets = []string{
	"linux/amd64", "linux/arm64", "linux/arm",
	"darwin/amd64", "darwin/arm64",
	"windows/amd64", "windows/arm64",
}

func main() {
	all := flag.Bool("all", false, "build for every platform")
	targetList := flag.String("target", "", "comma-separated os/arch list, e.g. linux/arm64,windows/amd64 (default: this machine)")
	matterMode := flag.String("matter", "auto", "embed the Matter bridge: auto (when Node.js is installed), on, or off")
	pkg := flag.Bool("package", false, "also pack each build with the README into a .tar.gz/.zip, like the releases")
	versionFlag := flag.String("version", "", "version to stamp (default: git describe)")
	flag.Parse()

	if err := run(*all, *targetList, *matterMode, *pkg, *versionFlag); err != nil {
		fmt.Fprintln(os.Stderr, "build failed:", err)
		os.Exit(1)
	}
}

func run(all bool, targetList, matterMode string, pkg bool, version string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}

	host := runtime.GOOS + "/" + runtime.GOARCH
	targets := []string{host}
	switch {
	case all:
		targets = allTargets
	case targetList != "":
		targets = strings.Split(targetList, ",")
	}
	for i, t := range targets {
		targets[i] = strings.TrimSpace(t)
		if !strings.Contains(targets[i], "/") {
			return fmt.Errorf("target %q: want os/arch, e.g. linux/arm64", t)
		}
	}
	// Building just this machine without packaging gives ./wiser-dashboard, as the README describes.
	local := len(targets) == 1 && targets[0] == host && !pkg

	if version == "" {
		version = gitVersion()
	}

	matter, err := matterReady(matterMode)
	if err != nil {
		return err
	}

	// Each build swaps its platform's bridge into matterbin/; put back what was there.
	embedPath := filepath.Join("matterbin", "wiser-matter")
	if saved, err := os.ReadFile(embedPath); err == nil {
		defer os.WriteFile(embedPath, saved, 0o755)
	} else {
		defer os.Remove(embedPath)
	}

	fmt.Printf("wiser-dashboard %s, Matter bridge: %s\n\n", version, map[bool]string{true: "yes", false: "no"}[matter])
	for _, t := range targets {
		goos, goarch, _ := strings.Cut(t, "/")
		out := filepath.Join("dist", "wiser-dashboard-"+goos+"-"+goarch)
		if local {
			out = "wiser-dashboard"
		}
		if goos == "windows" {
			out += ".exe"
		}

		tags := ""
		withMatter := matter && bunTarget(goos, goarch) != ""
		if withMatter {
			bridge, err := buildBridge(goos, goarch)
			if err != nil {
				return fmt.Errorf("%s: Matter bridge: %w (use -matter=off to build without it)", t, err)
			}
			if err := copyFile(bridge, embedPath, 0o755); err != nil {
				return err
			}
			tags = "matter"
		}

		start := time.Now()
		if err := goBuild(goos, goarch, tags, version, out); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		note := ""
		switch {
		case withMatter:
			note = ", with Matter"
		case matter:
			note = ", without Matter (Bun doesn't support " + t + ")"
		}
		fmt.Printf("  %-14s -> %s (%s%s, %s)\n", t, out, size(out), note, time.Since(start).Round(time.Second))

		if pkg {
			archive, err := packageBuild(out, goos, goarch, version)
			if err != nil {
				return fmt.Errorf("%s: packaging: %w", t, err)
			}
			fmt.Printf("  %-14s -> %s (%s)\n", "", archive, size(archive))
		}
	}
	return nil
}

// repoRoot finds the directory holding this project's go.mod, from wherever
// the script was started inside the repository.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.HasPrefix(string(b), "module wiser-dashboard") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run this inside the wiser-dashboard repository")
		}
		dir = parent
	}
}

func gitVersion() string {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "dev"
	}
	return strings.TrimSpace(string(out))
}

// matterReady reports whether the bridge can be built, installing its
// dependencies first if needed.
func matterReady(mode string) (bool, error) {
	switch mode {
	case "off":
		return false, nil
	case "on", "auto":
	default:
		return false, fmt.Errorf("-matter=%s: want auto, on or off", mode)
	}
	for _, tool := range []string{"node", "npm", "npx"} {
		if _, err := exec.LookPath(tool); err != nil {
			if mode == "on" {
				return false, errors.New("the Matter bridge needs Node.js (node, npm, npx): https://nodejs.org")
			}
			fmt.Println("Node.js not found: building without the Matter bridge (install Node.js to include it).")
			return false, nil
		}
	}
	// npm ci when node_modules is missing or older than the lockfile.
	lock, err := os.Stat(filepath.Join("matter", "package-lock.json"))
	if err != nil {
		return false, err
	}
	installed, err := os.Stat(filepath.Join("matter", "node_modules", ".package-lock.json"))
	if err != nil || installed.ModTime().Before(lock.ModTime()) {
		fmt.Println("Installing the Matter bridge's dependencies (npm ci)...")
		if err := runIn("matter", nil, "npm", "ci", "--no-audit", "--no-fund"); err != nil {
			return false, fmt.Errorf("npm ci: %w", err)
		}
	}
	return true, nil
}

// bunTarget is Bun's name for a platform, or "" where Bun has no build.
// "baseline" x64 builds also run on older CPUs (mini PCs, NAS boxes).
func bunTarget(goos, goarch string) string {
	switch goarch {
	case "amd64":
		return "bun-" + goos + "-x64-baseline"
	case "arm64":
		return "bun-" + goos + "-arm64"
	}
	return ""
}

// buildBridge compiles the bridge for one platform into matter/dist/.
func buildBridge(goos, goarch string) (string, error) {
	out := filepath.Join("dist", "wiser-matter-"+goos+"-"+goarch) // relative to matter/
	if err := runIn("matter", nil, "npx", "bun", "build", "--compile", "--target="+bunTarget(goos, goarch),
		filepath.Join("src", "main.ts"), "--outfile", out); err != nil {
		return "", err
	}
	path := filepath.Join("matter", out)
	if goos == "windows" {
		path += ".exe" // Bun adds it
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("bun produced no %s", path)
	}
	return path, nil
}

func goBuild(goos, goarch, tags, version, out string) error {
	args := []string{"build"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", out, ".")
	env := []string{"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch}
	if goarch == "arm" {
		env = append(env, "GOARM=7")
	}
	return runIn(".", env, "go", args...)
}

// runIn runs a command in dir with extra environment, passing its output through.
func runIn(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, buf.String())
	}
	return nil
}

// packageBuild packs a build with the README (and, for Linux, the systemd
// unit) the way the releases are: a .zip for Windows, .tar.gz otherwise.
func packageBuild(bin, goos, goarch, version string) (string, error) {
	name := "wiser-dashboard-" + version + "-" + goos + "-" + goarch
	if goarch == "arm" {
		name += "v7"
	}
	binName := "wiser-dashboard"
	if goos == "windows" {
		binName += ".exe"
	}
	files := []struct{ src, dst string }{{bin, binName}, {"README.md", "README.md"}}
	if goos == "linux" {
		files = append(files, struct{ src, dst string }{filepath.Join("deploy", "wiser-dashboard.service"), "wiser-dashboard.service"})
	}
	if err := os.MkdirAll("dist", 0o755); err != nil {
		return "", err
	}

	if goos == "windows" {
		path := filepath.Join("dist", name+".zip")
		f, err := os.Create(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		zw := zip.NewWriter(f)
		for _, fl := range files {
			h := &zip.FileHeader{Name: name + "/" + fl.dst, Method: zip.Deflate, Modified: time.Now()}
			h.SetMode(0o755)
			w, err := zw.CreateHeader(h)
			if err != nil {
				return "", err
			}
			if err := copyInto(w, fl.src); err != nil {
				return "", err
			}
		}
		return path, zw.Close()
	}

	path := filepath.Join("dist", name+".tar.gz")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, fl := range files {
		st, err := os.Stat(fl.src)
		if err != nil {
			return "", err
		}
		mode := int64(0o644)
		if fl.dst == binName {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + fl.dst, Mode: mode, Size: st.Size(), ModTime: time.Now()}); err != nil {
			return "", err
		}
		if err := copyInto(tw, fl.src); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return path, gz.Close()
}

func copyInto(w io.Writer, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

func size(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%.1f MB", float64(st.Size())/(1<<20))
}
