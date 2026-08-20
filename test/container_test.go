//go:build container

// Package test holds the container smoke assertions for the sentinel
// image (contracts/runtime.md R8, table C1-C13). Run with:
//
//	go test -tags container ./test/...
//
// Every case prints PASS/FAIL/SKIP explicitly (R8: "a SKIP is explicit,
// never a silent pass"). These tests shell out to `docker` (a Podman shim
// is fine locally per CLAUDE.md) and to the repo root's deploy/ artifacts;
// they are NOT run by `go test ./...` (no build tag) or by CI's `test` job
// (R6) — only by a dedicated container job/local run on a real Linux host.
//
// Architecture is a property of the process running this suite, not
// something the suite loops over: this file tests the platform it is
// actually running ON (runtime.GOARCH, below), natively — never both,
// never under emulation. Coverage of BOTH linux/amd64 and linux/arm64
// (contracts/runtime.md R1) comes from running this suite once per
// architecture, on a runner native to each, which is the CI workflow's
// job via its build matrix, not this file's.
//
// AGY_URL_AMD64/AGY_SHA512_AMD64 and AGY_URL_ARM64/AGY_SHA512_ARM64: real
// ops input (contracts/runtime.md R1), never guessed here. If the pair
// matching this process's own architecture is set in the environment,
// the real tarball is used. Otherwise a SYNTHETIC local fixture (a fake
// "agy" binary, cross-compiled for this architecture and served by an
// in-process httptest.Server) stands in, so the Dockerfile's OWN
// mechanics (download, sha512 verify, unpack, permissions, build-time
// verification) are still exercised end to end without ever touching a
// real or guessed download location. If neither path is reachable
// (e.g. a sandboxed CI runner with no container-to-host networking),
// every case SKIPs loudly with the reason.
package test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// arch is this process's own architecture — the runner's. The workflow
// runs this suite once per platform, each on a runner native to that
// platform (no more looping over both architectures inside one process,
// and nothing here to emulate), so runtime.GOARCH IS the platform under
// test. It is also, conveniently, exactly the string Docker's
// --platform linux/<arch> expects ("amd64"/"arm64"), so it needs no
// translation.
var arch = runtime.GOARCH

// imageTag is the image this process builds and every test in it reuses
// (sync.Once via requireImage). Tests in this file are never
// t.Parallel (grep confirms none are), so this and arch being plain
// package vars is safe.
var imageTag = "sentinel:container-test-" + arch

// logPass logs a "PASS ..." line only when nothing in this test has
// already failed. An unconditional t.Log("PASS ...") after a loop of
// non-fatal t.Errorf calls prints PASS right below the FAIL lines it
// contradicts — misleading in a document meant to be a gate record.
func logPass(t *testing.T, format string, args ...any) {
	t.Helper()
	if !t.Failed() {
		t.Logf(format, args...)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func dockerBin() string {
	if b := os.Getenv("SENTINEL_DOCKER_BIN"); b != "" {
		return b
	}
	return "docker"
}

func runCmd(t *testing.T, timeout time.Duration, name string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// --- shared image build (once per test binary run) ---

type fakeAgyFixture struct {
	srv    *httptest.Server
	url    string
	sha512 string
}

var (
	fakeAgy   *fakeAgyFixture
	fakeAgyMu sync.Mutex
)

// fakeAgyMain is a minimal Go program cross-compiled into a REAL,
// architecture-correct static ELF binary — not a #!/bin/sh script. C1's
// coherence check reads agy's own ELF e_machine, since that is the
// value a wrong URL/digest pair would corrupt silently, and a shell
// script has no ELF header at all for that check to read — an empty
// header reads as a mismatch on every run, synthetic-fixture or not. A
// cross-compiled Go binary is real ELF, for whichever GOARCH it was
// built with, so the same coherence check that verifies the real
// vendor artifact also verifies this fixture, including when it is
// deliberately cross-compiled for the WRONG architecture to prove the
// check actually fires.
const fakeAgySrc = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("agy container-test 0.0.0-test")
	}
}
`

// syntheticAgy starts an in-process HTTP server serving a fake agy
// tarball CROSS-COMPILED for this process's own arch, and returns its
// URL + sha512. Purely local — never a guessed remote location. Built
// once and cached (sync via fakeAgyMu, same pattern as buildOnce).
//
// The fixture executable is deliberately named "not-agy", NOT "agy"
// (contracts/runtime.md R1): the real vendor tarball contains a single
// ELF executable named "antigravity" — the vendor's own installer is
// what renames it to "agy" on install. A fixture literally named "agy"
// would let the extraction step match on filename and mask the case
// where the real archive has no such name to match. This fixture
// exercises the
// same shape: one executable, not named "agy", found by permission bit.
func syntheticAgy(t *testing.T) (url, sha string) {
	t.Helper()
	fakeAgyMu.Lock()
	defer fakeAgyMu.Unlock()
	if fakeAgy != nil {
		return fakeAgy.url, fakeAgy.sha512
	}
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(fakeAgySrc), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "not-agy")
	buildCmd := exec.Command("go", "build", "-o", binPath, srcPath)
	buildCmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	var buildOut bytes.Buffer
	buildCmd.Stdout = &buildOut
	buildCmd.Stderr = &buildOut
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("cross-compiling the fake agy fixture for %s: %v\n%s", arch, err, buildOut.String())
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "agy.tar.gz")
	// COPYFILE_DISABLE=1: macOS's tar otherwise embeds an AppleDouble
	// sidecar file (._not-agy) alongside the real one — extracted on
	// Linux it ALSO carries the executable bit, so the Dockerfile's
	// "exactly one executable" check (correctly) refuses to guess and
	// fails the build with 2 candidates. That is the Dockerfile doing
	// its job; the sidecar file is purely an artifact of building this
	// FIXTURE on macOS and has nothing to do with the real vendor
	// tarball, so it belongs suppressed here, not tolerated there.
	t.Setenv("COPYFILE_DISABLE", "1")
	if out, errOut, code := runCmd(t, 30*time.Second, "tar", "-czf", tarPath, "-C", dir, "not-agy"); code != 0 {
		t.Fatalf("tar: %s %s", out, errOut)
	}
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)
	shaHex := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/agy.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	fakeAgy = &fakeAgyFixture{srv: srv, url: srv.URL + "/agy.tar.gz", sha512: shaHex}
	return fakeAgy.url, fakeAgy.sha512
}

// buildSentinelImage builds deploy/Dockerfile once, tagged imageTag, for
// --platform linux/<arch> (this process's own arch). Real
// AGY_URL_<ARCH>/AGY_SHA512_<ARCH> win if set for THIS arch; otherwise
// the synthetic fixture above is used with --add-host so the builder
// container can reach this process. The fixture is a cross-compiled ELF
// built for this arch (see syntheticAgy). Every case that needs a built
// image calls this first (via requireImage) and SKIPs (never fails) if
// it returns an error — an unreachable Docker daemon or a sandboxed CI
// runner without container-to-host networking is an environment
// limitation, not a defect in the Dockerfile.
var (
	buildOnce sync.Once
	buildErr  error
)

func buildSentinelImage(t *testing.T) error {
	t.Helper()
	buildOnce.Do(func() {
		root := repoRoot(t)
		envURL := "AGY_URL_" + strings.ToUpper(arch)
		envSHA := "AGY_SHA512_" + strings.ToUpper(arch)
		url := os.Getenv(envURL)
		sha := os.Getenv(envSHA)
		hostFlag := ""
		if url == "" || sha == "" {
			url, sha = syntheticAgy(t)
			// host.docker.internal works with --add-host=host.docker.internal:host-gateway
			// on real Docker (20.10+); Podman resolves host.containers.internal
			// natively. Try docker.internal first (matches what CI's real
			// docker will use); a Podman shim locally also honors it with the
			// add-host flag.
			url = strings.Replace(url, "127.0.0.1", "host.docker.internal", 1)
			hostFlag = "host.docker.internal:host-gateway"
		}
		args := []string{"build", "-f", "deploy/Dockerfile", "-t", imageTag,
			"--platform", "linux/" + arch,
			"--build-arg", "AGY_URL_AMD64=" + pick("amd64", url),
			"--build-arg", "AGY_SHA512_AMD64=" + pick("amd64", sha),
			"--build-arg", "AGY_URL_ARM64=" + pick("arm64", url),
			"--build-arg", "AGY_SHA512_ARM64=" + pick("arm64", sha),
			"--build-arg", "VERSION=container-test",
		}
		if hostFlag != "" {
			args = append(args, "--add-host", hostFlag)
		}
		args = append(args, ".")
		cmd := exec.Command(dockerBin(), args...)
		cmd.Dir = root
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			buildErr = fmt.Errorf("docker build --platform linux/%s failed: %w\n%s", arch, err, out.String())
		}
	})
	return buildErr
}

// pick returns val when this process's own arch == want, else "" — used
// to leave the OTHER architecture's build-arg pair empty (the Dockerfile
// only requires and uses whichever pair matches TARGETARCH for this
// build, so the unused pair being empty is correct, not a gap: it proves
// the per-arch selection actually selects rather than accepting whatever
// is present).
func pick(want, val string) string {
	if arch == want {
		return val
	}
	return ""
}

// dockerAvailable is a cheap, cached "is the Docker/Podman daemon even
// reachable" probe, checked once per test binary run — kept separate
// from a build failure so an unreachable daemon SKIPs while a real
// Dockerfile regression still FAILs, rather than both collapsing into
// the same "not a Dockerfile defect" SKIP.
var (
	dockerAvailOnce sync.Once
	dockerAvailErr  error
)

func dockerAvailable(t *testing.T) error {
	t.Helper()
	dockerAvailOnce.Do(func() {
		_, errOut, code := runCmd(t, 10*time.Second, dockerBin(), "version")
		if code != 0 {
			dockerAvailErr = fmt.Errorf("%s not reachable: %s", dockerBin(), errOut)
		}
	})
	return dockerAvailErr
}

// requireImage builds (and caches, sync.Once) this process's own-arch
// image. Every test in this file calls it first.
func requireImage(t *testing.T) {
	t.Helper()
	if err := dockerAvailable(t); err != nil {
		t.Skipf("SKIP: %v (environment limitation)", err)
	}
	// Daemon IS reachable past this point — a build failure from here on
	// is either a real Dockerfile/build regression (FAIL) or the
	// synthetic-agy-fixture's own container-to-host networking not
	// working in this sandbox (still a real thing to know, so it FAILs
	// too rather than silently vanishing as a SKIP — the AGY_URL-missing
	// case, the one truly expected failure without ops input, has its
	// own dedicated test elsewhere and is not what requireImage guards).
	if err := buildSentinelImage(t); err != nil {
		t.Fatalf("FAIL: could not build %s (linux/%s): %v", imageTag, arch, err)
	}
}

// wantELFMachine returns the little-endian e_machine bytes (as `od`
// prints them, low byte first) a well-formed binary for arch must have.
func wantELFMachine(arch string) string {
	switch arch {
	case "amd64":
		return "3e 00" // EM_X86_64 = 62
	case "arm64":
		return "b7 00" // EM_AARCH64 = 183
	default:
		return "??"
	}
}

// assertELFMachine reads the real ELF e_machine field (a little-endian
// uint16 at byte offset 18) out of binPath inside the given image and
// fails the test unless it matches arch. `od` (coreutils) is used
// because `file` is not installed in the runtime image (R1's explicit
// package list). This is one of the three coherence signals
// contracts/runtime.md R1 requires C1 to check: "the
// Go binary's ELF machine, the image manifest's architecture, and the
// agy binary's ELF machine all agree with the platform that was
// requested."
func assertELFMachine(t *testing.T, tag, binPath, arch string) {
	t.Helper()
	out, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", "--entrypoint", "od",
		tag, "-An", "-tx1", "-j18", "-N2", binPath)
	if code != 0 {
		t.Fatalf("FAIL: reading ELF e_machine from %s failed (code=%d): %s %s", binPath, code, out, errOut)
	}
	want := wantELFMachine(arch)
	if machine := strings.Join(strings.Fields(out), " "); machine != want {
		t.Fatalf("FAIL: %s ELF e_machine = %q, want %q for linux/%s — architecture mismatch", binPath, machine, want, arch)
	}
}

// TestContainer_RealAgyBuild builds the image with real, vendor-published
// agy values — the one check that catches an extraction bug no synthetic
// fixture can, since a synthetic tarball's layout is exactly the layout
// the code was written against. Gated on SENTINEL_REAL_AGY=1 (same gate
// C9/CLAUDE.md already use for real-agy interaction) since it downloads
// the real tarball (~53-56 MB, ~200 MB extracted) from the vendor over
// the network — not something a default `go test ./...` or even the
// default container suite should do.
//
// The values below were independently fetched and sha512-checked on
// 2026-08-19 for agy 1.1.15, one pair per architecture. They are NOT
// Dockerfile/compose defaults — R1 requires AGY_URL_<ARCH>/
// AGY_SHA512_<ARCH> as ops input with no default, on purpose — these are
// fixture literals for this one gated verification, the same way a
// table-driven test's fixture data is not a production default just
// because it lives in the repo.
var realAgyValues = map[string]struct{ url, sha512, version string }{
	"amd64": {
		url:     "https://storage.googleapis.com/antigravity-public/antigravity-cli/1.1.15-5350383476932608/linux-x64/cli_linux_x64.tar.gz",
		sha512:  "7d6020caff2e06a5ddf2553f2d9d5b428e3becc69727d11032f10e40609b938db428578ccd5c72694bab5e4da483de9ad3121578cc172adf174aa5263ce51dcc",
		version: "1.1.15",
	},
	"arm64": {
		url:     "https://storage.googleapis.com/antigravity-public/antigravity-cli/1.1.15-5350383476932608/linux-arm/cli_linux_arm64.tar.gz",
		sha512:  "2571031ded807a624fad5166bfb7ee2eb0c97862480fc423da673fde2025b71a35240d213ece6a42ad44d215959fa050a71750c76f358befdd245f5933c4a104",
		version: "1.1.15",
	},
}

func TestContainer_RealAgyBuild(t *testing.T) {
	if os.Getenv("SENTINEL_REAL_AGY") != "1" {
		t.Skip("SKIP: SENTINEL_REAL_AGY != 1 — set it to build the image against the REAL agy tarballs over the network (~53-56MB download each, ~200MB extracted each)")
	}

	real := realAgyValues[arch]

	// This URL is pinned to one specific vendor release rather than
	// re-resolved from the manifest each run, so when the vendor
	// rotates it this URL can 404 for a reason that has nothing to do
	// with this repo. Distinguish "the pinned artifact is gone" (SKIP,
	// loudly, same as requireImage's daemon-unreachable case) from "the
	// build itself is broken" (FAIL) with a cheap reachability check
	// before spending minutes on a doomed build.
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if resp, err := httpClient.Head(real.url); err != nil {
		t.Skipf("SKIP: %s unreachable (%v) — pinned to agy %s (%s), needs a refresh if the vendor has rotated past it", arch, err, real.version, arch)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Skipf("SKIP: %s returned HTTP %d — pinned to agy %s (%s), needs a refresh if the vendor has rotated past it", arch, resp.StatusCode, real.version, arch)
		}
	}

	root := repoRoot(t)
	tag := "sentinel:real-agy-test-" + arch

	// Both architectures' build-arg pairs are always passed (the unused
	// one just goes unread by this build's TARGETARCH branch in the
	// Dockerfile) — this exercises the exact per-arch selection logic a
	// real `docker buildx build --platform linux/amd64,linux/arm64`
	// invocation would hit, not a simplified single-pair path.
	args := []string{"build", "-f", "deploy/Dockerfile", "-t", tag,
		"--platform", "linux/" + arch,
		"--build-arg", "AGY_URL_AMD64=" + realAgyValues["amd64"].url,
		"--build-arg", "AGY_SHA512_AMD64=" + realAgyValues["amd64"].sha512,
		"--build-arg", "AGY_URL_ARM64=" + realAgyValues["arm64"].url,
		"--build-arg", "AGY_SHA512_ARM64=" + realAgyValues["arm64"].sha512,
		"--build-arg", "AGY_VERSION=" + real.version,
		"--build-arg", "VERSION=real-agy-test",
		".",
	}
	// -f is relative to the docker CLI's own cwd, and "." is the build
	// context — both need cmd.Dir=root, exactly like buildSentinelImage
	// does; runCmd (used everywhere else in this file) has no cwd
	// override and would resolve "deploy/Dockerfile" against the test
	// binary's own working directory instead.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBin(), args...)
	cmd.Dir = root
	var buildOut bytes.Buffer
	cmd.Stdout = &buildOut
	cmd.Stderr = &buildOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("FAIL: real-agy build (linux/%s) failed: %v\n%s", arch, err, buildOut.String())
	}
	defer runCmd(t, 30*time.Second, dockerBin(), "rmi", tag)

	// The real agy --version output and a real sentinel --version, both
	// out of the ACTUAL built image — not the synthetic fixture's stub.
	verOut, verErr, verCode := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", "--entrypoint", "agy", tag, "--version")
	if verCode != 0 {
		t.Fatalf("FAIL: agy --version in the real-agy %s image (code=%d): %s %s", arch, verCode, verOut, verErr)
	}
	t.Logf("real agy --version (%s): %s", arch, strings.TrimSpace(verOut))

	sentOut, sentErr, sentCode := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", tag, "--version")
	if sentCode != 0 {
		t.Fatalf("FAIL: sentinel --version in the real-agy %s image (code=%d): %s %s", arch, sentCode, sentOut, sentErr)
	}

	// Coherence check with the REAL binaries — this is the one main
	// named explicitly: "Add agy's ELF to the check — that is the one
	// this whole round exists over, and it is the piece a wrong URL
	// pair would corrupt silently."
	archOut, archErr, archCode := runCmd(t, 15*time.Second, dockerBin(), "image", "inspect", tag, "--format", "{{.Architecture}}")
	if archCode != 0 {
		t.Fatalf("FAIL: docker image inspect --format {{.Architecture}} failed for real-agy %s (code=%d): %s %s", arch, archCode, archOut, archErr)
	}
	if got := strings.TrimSpace(archOut); got != arch {
		t.Fatalf("FAIL: real-agy %s image Architecture = %q, want %q", arch, got, arch)
	}
	assertELFMachine(t, tag, "/usr/local/bin/sentinel", arch)
	assertELFMachine(t, tag, "/usr/local/bin/agy", arch)

	sizeOut, _, sizeCode := runCmd(t, 10*time.Second, dockerBin(), "image", "inspect", tag, "--format", "{{.Size}}")
	if sizeCode == 0 {
		if sizeBytes, err := strconv.ParseInt(strings.TrimSpace(sizeOut), 10, 64); err == nil {
			t.Logf("real-agy %s image size: %d bytes (%.1f MB) — this is what a real host pulls", arch, sizeBytes, float64(sizeBytes)/1e6)
		}
	}
	logPass(t, "PASS real-agy build (%s, version=%s)", arch, real.version)
}

// --- C1: container starts unprivileged ---

func TestContainer_C1_StartsUnprivileged(t *testing.T) {
	requireImage(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "--entrypoint", "id", imageTag, "-u")
	if code != 0 || strings.TrimSpace(out) != "10001" {
		t.Fatalf("FAIL C1: id -u = %q (code=%d) stderr=%q, want 10001/0", out, code, errOut)
	}
	out, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", imageTag, "--version")
	if code != 0 || !strings.Contains(out, "container-test") {
		t.Fatalf("FAIL C1: sentinel --version = %q (code=%d), want to contain the stamped version", out, code)
	}
	// A host executes a static ELF matching its OWN architecture
	// regardless of an image's declared platform — platform is
	// manifest metadata, not an execution sandbox — so a stage that
	// cross-compiles for one architecture while another stage is
	// frozen at a different one can produce an image whose
	// build-time `--version` check still passes cleanly. Because
	// arm64 is a legitimate build target here, not just amd64,
	// asserting a CONSTANT architecture would fail every legitimate
	// arm64 build. What must hold instead is COHERENCE: the image
	// manifest's declared architecture, the Go binary's real ELF
	// machine, AND agy's own real ELF machine must all agree with the
	// platform this process is actually running on (arch, the package
	// var). agy's ELF matters because the extraction step never
	// inspects the architecture of what it downloads — a mismatched
	// AGY_URL_<ARCH>/AGY_SHA512_<ARCH> pairing would corrupt agy
	// silently while sentinel and the image label both stayed correct.
	// This default (synthetic-fixture) path only ever passes the pair
	// matching TARGETARCH (see pick() above), so it catches a MISSING
	// pair loudly but cannot exercise a pair that is present yet
	// mismatched — only TestContainer_RealAgyBuild, which supplies
	// both real pairs at once, can catch a genuinely wrong-but-present
	// binary.
	archOut, archErr, archCode := runCmd(t, 15*time.Second, dockerBin(), "image", "inspect", imageTag, "--format", "{{.Architecture}}")
	if archCode != 0 {
		t.Fatalf("FAIL C1: docker image inspect --format {{.Architecture}} failed (code=%d): %s %s", archCode, archOut, archErr)
	}
	if got := strings.TrimSpace(archOut); got != arch {
		t.Fatalf("FAIL C1: image Architecture = %q, want %q (built --platform linux/%s)", got, arch, arch)
	}
	assertELFMachine(t, imageTag, "/usr/local/bin/sentinel", arch)
	assertELFMachine(t, imageTag, "/usr/local/bin/agy", arch)
	logPass(t, "PASS C1 (%s)", arch)
}

// --- C3: every ro mount rejects a write; /state and /tmp accept one ---

// hostPathExists checks existence via a throwaway container bind-mounting
// the Docker/Podman HOST's path — never the local Go test process's own
// filesystem (the C2/C11 lesson: on a Podman Desktop/machine setup this
// test binary runs on macOS while containers run in a separate Linux VM).
//
// Mounts the PARENT of hostPath, never hostPath itself: a rootful daemon
// creates a missing bind-mount SOURCE directory on the host before the
// container starts, so probing a path that does not exist yet would
// itself create it — this test must never mutate the machine it runs
// on. Every real R4 mount target's parent (/var/log, /run/log,
// /var/lib, /, /etc) is guaranteed to exist, so mounting the parent
// never triggers that auto-create, and testing for the child by name
// inside the container is a pure read.
func hostPathExists(t *testing.T, hostPath string) bool {
	t.Helper()
	if hostPath == "/" {
		return true
	}
	parent := filepath.Dir(hostPath)
	base := filepath.Base(hostPath)
	_, _, code := runCmd(t, 10*time.Second, dockerBin(), "run", "--rm",
		"-v", parent+":/probe-parent:ro", "debian:trixie-slim", "test", "-e", "/probe-parent/"+base)
	return code == 0
}

// TestContainer_C3_ReadOnlySurfaces is R8 C3: "for EVERY ro mount target
// of R4, creating a file fails". Running `docker run --read-only` with
// no bind mounts attached would make most "write failed" assertions
// actually "path does not exist" — green evidence for an assertion
// never made — so this attaches the REAL R4 mount set (real host paths,
// read-only) so a bind that lost its `:ro` in compose is actually
// caught here.
func TestContainer_C3_ReadOnlySurfaces(t *testing.T) {
	requireImage(t)
	selinux := selinuxEnforcingOnDockerHost(t)

	// {container target, real host source, isDir}. Matches R4's mount
	// list exactly (minus AGY_SECRET_DIR and /state/tmpfs, which are not
	// "ro mount targets" — /state and /tmp are the rw exception C3 checks
	// separately below).
	type mount struct {
		target string
		host   string
		isDir  bool
	}
	mounts := []mount{
		{"/host/journal", "/var/log/journal", true},
		{"/host/journal-volatile", "/run/log/journal", true},
		{"/etc/machine-id", "/etc/machine-id", false},
		{"/host/rasdaemon", "/var/lib/rasdaemon", true},
		{"/host/proc", "/proc", true},
		{"/host/sys", "/sys", true},
		{"/etc/os-release", "/etc/os-release", false},
		{"/host/root", "/", true},
	}

	for _, m := range mounts {
		if !hostPathExists(t, m.host) {
			t.Logf("SKIP C3 target %s: host path %s does not exist on this test host", m.target, m.host)
			continue
		}
		var writeTarget string
		if m.isDir {
			writeTarget = strings.TrimRight(m.target, "/") + "/.w"
		} else {
			writeTarget = m.target // overwrite attempt on the file itself
		}
		script := fmt.Sprintf("echo x > %s 2>/dev/null; echo rc=$?", writeTarget)
		args := []string{"run", "--rm",
			"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
			"-u", "10001:10001", "--tmpfs", "/tmp",
			"-v", m.host + ":" + m.target + ":ro"}
		if selinux {
			args = append(args, "--security-opt", "label=disable")
		}
		args = append(args, "--entrypoint", "sh", imageTag, "-c", script)
		out, errOut, _ := runCmd(t, 15*time.Second, dockerBin(), args...)
		if !strings.Contains(out, "rc=") || strings.Contains(out, "rc=0") {
			t.Errorf("FAIL C3: write to %s (host %s) did not fail as expected: out=%q err=%q", m.target, m.host, out, errOut)
		}
	}

	// /usr/local/bin has no host mount — it's part of the image itself,
	// and read_only:true is what protects it.
	out, _, _ := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /usr/local/bin/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=") || strings.Contains(out, "rc=0") {
		t.Errorf("FAIL C3: write to /usr/local/bin did not fail as expected: %q", out)
	}

	// /state and /tmp are the two rw exceptions — must accept a write
	// (R8 C3: "/state/.w and /tmp/.w succeed").
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o777); err != nil { // container uid 10001 != host owner uid
		t.Fatal(err)
	}
	out, _, _ = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp", "-v", stateDir+":/state",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /state/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=0") {
		t.Fatalf("FAIL C3: write to /state should succeed: %q", out)
	}
	out, _, _ = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /tmp/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=0") {
		t.Fatalf("FAIL C3: write to /tmp should succeed: %q", out)
	}
	logPass(t, "PASS C3")
}

// --- C2: journal readable via group_add ---
//
// Prefers the REAL host journal (/var/log/journal) when this test host has
// one and journalctl is on PATH — the actual R8 C2 command,
// "journalctl -D /host/journal -n1", against actual binary journal files,
// gated on the actual systemd-journal gid instead of any belief about it.
// "--security-opt label=disable" is added only when SELinux is enforcing
// on the Docker/Podman host itself (common on a Podman Desktop/machine VM)
// — that flag accommodates THIS dev sandbox's mandatory access control on
// bind mounts, is not part of any shipped compose/Dockerfile artifact, and
// bam (plain Debian, no SELinux) does not need it.
//
// Falls back to a synthetic POSIX-permission probe (a throwaway gid file,
// not a real journal) when no real host journal is reachable — a Linux CI
// runner may have no active journald — so the group_add MECHANISM is still
// exercised even without real journal content. SKIPs loudly, never
// silently, when neither path is set up on this host (C9).
func TestContainer_C2_JournalViaGroupAdd(t *testing.T) {
	requireImage(t)
	if gid, ok := hostSystemdJournalGID(t); ok {
		selinux := selinuxEnforcingOnDockerHost(t)
		base := []string{"run", "--rm", "-v", "/var/log/journal:/host/journal:ro",
			"-u", "10001:10001", "--entrypoint", "journalctl"}
		if selinux {
			base = append(base, "--security-opt", "label=disable")
		}
		withoutArgs := append(append([]string{}, base...), imageTag, "-D", "/host/journal", "-n1", "-o", "json", "--no-pager")
		_, _, codeWithout := runCmd(t, 15*time.Second, dockerBin(), withoutArgs...)

		withArgs := append(append([]string{}, base[:2]...), append([]string{"--group-add", gid}, base[2:]...)...)
		withArgs = append(withArgs, imageTag, "-D", "/host/journal", "-n1", "-o", "json", "--no-pager")
		outWith, _, codeWith := runCmd(t, 15*time.Second, dockerBin(), withArgs...)

		if codeWithout == 0 {
			t.Log("NOTE C2: reading the real journal succeeded even without --group-add on this host — cannot prove the negative direction here, falling through to the positive assertion only")
		}
		if codeWith != 0 || strings.TrimSpace(outWith) == "" {
			t.Fatalf("FAIL C2: --group-add %s could not read the real host journal (code=%d): %s", gid, codeWith, outWith)
		}
		logPass(t, "PASS C2 (real host journal, real systemd-journal gid)")
		return
	}

	// Fallback: synthetic POSIX-permission probe.
	dir := t.TempDir()
	if out, errOut, code := runCmd(t, 10*time.Second, "chmod", "0750", dir); code != 0 {
		t.Skipf("SKIP C2: chmod on host failed: %s %s", out, errOut)
	}
	const testGID = 54321 // throwaway, unlikely to collide with a real system group
	if out, errOut, code := runCmd(t, 10*time.Second, "chown", ":"+fmt.Sprint(testGID), dir); code != 0 {
		t.Skipf("SKIP C2: chown to gid %d failed (needs root or matching group on this host): %s %s", testGID, out, errOut)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := runCmd(t, 10*time.Second, "chown", ":"+fmt.Sprint(testGID), filepath.Join(dir, "probe")); code != 0 {
		t.Skipf("SKIP C2: chown probe file failed: %s %s", out, errOut)
	}

	_, _, codeWithout := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-u", "10001:10001", "-v", dir+":/probe:ro",
		"--entrypoint", "cat", imageTag, "/probe/probe")
	_, _, codeWith := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-u", "10001:10001", "--group-add", fmt.Sprint(testGID),
		"-v", dir+":/probe:ro",
		"--entrypoint", "cat", imageTag, "/probe/probe")

	if codeWithout == 0 {
		t.Skip("SKIP C2: this host does not enforce the group permission boundary the way a real Linux target does (probe was readable even without group_add) — inconclusive here, needs live-host validation")
	}
	if codeWith != 0 {
		t.Errorf("FAIL C2: --group-add %d still could not read the probe file (code=%d)", testGID, codeWith)
	} else {
		logPass(t, "PASS C2 (synthetic gid fallback — no real host journal reachable in this environment)")
	}
}

// hostSystemdJournalGID discovers the numeric group that owns the
// Docker/Podman host's real /var/log/journal, if one exists — via a
// throwaway container's `stat`, NOT by running getent/stat on the local Go
// test process. On a Podman Desktop/machine setup (this dev sandbox), the
// test binary runs on macOS while containers run inside a Linux VM with
// its own filesystem; checking the test process's own OS would always
// report "no journal" even when the VM genuinely has one, which is
// exactly what the first version of this test got wrong. Stat'ing the
// bind-mounted directory's owning gid also sidesteps needing /etc/group
// name resolution to agree between host and container.
func hostSystemdJournalGID(t *testing.T) (string, bool) {
	t.Helper()
	out, _, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", "/var/log/journal:/hj:ro",
		"debian:trixie-slim", "stat", "-c", "%g", "/hj")
	gid := strings.TrimSpace(out)
	if code != 0 || gid == "" {
		return "", false
	}
	return gid, true
}

// selinuxEnforcingOnDockerHost checks the machine actually running
// containers (relevant for a Podman Desktop VM on macOS, where the test
// process itself is NOT inside that VM) via `docker run --privileged`
// reading /sys/fs/selinux/enforce; best-effort, defaults to false (no
// extra flag) when it cannot tell.
func selinuxEnforcingOnDockerHost(t *testing.T) bool {
	t.Helper()
	out, _, code := runCmd(t, 10*time.Second, dockerBin(), "run", "--rm", "--privileged",
		"-v", "/sys/fs/selinux:/sys/fs/selinux:ro",
		"debian:trixie-slim", "sh", "-c", "cat /sys/fs/selinux/enforce 2>/dev/null")
	return code == 0 && strings.TrimSpace(out) == "1"
}

// --- C4: sensors -j ---
func TestContainer_C4_SensorsJSON(t *testing.T) {
	requireImage(t)
	// sensors reads the container's OWN /sys (there is no CLI flag to
	// point it at an arbitrary root), which under Docker/Podman is
	// normally the real host's sysfs shared via the kernel — no /host/sys
	// remapping needed for the standalone binary (that mapping is for
	// collect's own code, contracts/collect.md). On a sandboxed dev VM
	// with no physical sensor chips exposed, `sensors -j` prints "{}" AND
	// exits 1 ("No sensors found!") — that combination is a legitimate
	// "nothing detected" environment limitation, not a Dockerfile defect,
	// so it SKIPs rather than fails; any other non-zero exit is a real
	// failure.
	out, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--entrypoint", "sensors", imageTag, "-j")
	if code != 0 {
		if strings.TrimSpace(out) == "{}" {
			t.Skipf("SKIP C4: sensors -j found no chips in this environment (exit=%d, stderr=%q) — ARCHITECTURE §2.6 unverified point, needs live-host validation (e.g. bam)", code, errOut)
		}
		t.Fatalf("FAIL C4: sensors -j exit code = %d: %s %s", code, out, errOut)
	}
	m := mustJSON(t, out)
	if len(m) == 0 {
		t.Skip("SKIP C4: sensors -j returned an empty object — no hwmon sensor chips detected in this environment (ARCHITECTURE §2.6 unverified point; needs live-host validation, e.g. bam)")
	}
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err != nil || len(entries) == 0 {
		t.Skip("SKIP C4: /host/sys/class/hwmon not readable from the test process itself — cannot cross-check device names here")
	}
	var names []string
	for _, e := range entries {
		if nb, err := os.ReadFile(filepath.Join("/sys/class/hwmon", e.Name(), "name")); err == nil {
			names = append(names, strings.TrimSpace(string(nb)))
		}
	}
	found := false
	for k := range m {
		for _, n := range names {
			if strings.Contains(k, n) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("FAIL C4: none of sensors -j's keys (%v) matched a hwmon device name (%v)", keysOf(m), names)
	} else {
		logPass(t, "PASS C4")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- C5: rasdaemon path listable ---
func TestContainer_C5_RasdaemonListable(t *testing.T) {
	requireImage(t)
	if _, err := os.Stat("/var/lib/rasdaemon"); err != nil {
		t.Skip("SKIP C5: rasdaemon not present on this test host")
	}
	out, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", "/var/lib/rasdaemon:/host/rasdaemon:ro",
		"--entrypoint", "ls", imageTag, "/host/rasdaemon")
	if code != 0 {
		t.Fatalf("FAIL C5: ls /host/rasdaemon: %s %s", out, errOut)
	}
	logPass(t, "PASS C5")
}

// --- C7: ZED events under -t zed (0 hits is a pass) ---
func TestContainer_C7_ZedUnderJournalctl(t *testing.T) {
	requireImage(t)
	dir := t.TempDir()
	_, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", dir+":/host/journal:ro",
		"--entrypoint", "journalctl", imageTag, "-D", "/host/journal", "-t", "zed", "-n5")
	if code != 0 {
		t.Fatalf("FAIL C7: journalctl -t zed exit code = %d: %s", code, errOut)
	}
	logPass(t, "PASS C7 (0 hits on an empty synthetic journal dir is itself a pass per R8)")
}

// --- C8: smartd decode (no NVMe -> SKIP) ---
func TestContainer_C8_SmartdDecode(t *testing.T) {
	requireImage(t)
	dir := t.TempDir()
	_, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", dir+":/host/journal:ro",
		"--entrypoint", "journalctl", imageTag, "-D", "/host/journal", "-t", "smartd")
	if code != 0 {
		t.Fatalf("FAIL C8: journalctl -t smartd exit code = %d: %s", code, errOut)
	}
	t.Skip("SKIP C8: no NVMe/smartd fixture data available in this environment (0-hit decode above is not itself the assertion — the contract wants a synthetic 'Killed process' entry to reach the kernel section, which needs a real journal fixture; deferred to internal/collect's own hermetic tests, which already cover kernel-section parsing with testdata/bin)")
}

// --- C6: tmpfs/DNS ok under read_only ---

func TestContainer_C6_TmpfsAndTZ(t *testing.T) {
	requireImage(t)
	out, _, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /tmp/.w && echo tmp_ok; echo TZ=$TZ")
	if code != 0 || !strings.Contains(out, "tmp_ok") {
		t.Fatalf("FAIL C6: tmpfs write failed: %q (code=%d)", out, code)
	}
	if !strings.Contains(out, "TZ=UTC") {
		t.Fatalf("FAIL C6: TZ != UTC: %q", out)
	}
	logPass(t, "PASS C6 (DNS resolution of `apprise` requires the compose network — asserted separately via `docker compose config`, not a bare `docker run`)")
}

// --- C9: sentinel tick exit codes ---

func TestContainer_C9_TickExitCodes(t *testing.T) {
	requireImage(t)
	stateDir := t.TempDir()
	// state.New(cfg) creates active-alerts/history/outbox under
	// STATE_DIR at 0700 (internal/state/state.go) BEFORE any of the
	// config/journal preflight checks these tick invocations exit on
	// — every one of them reaches that far, not just the ones that
	// go on to succeed. Same reclaim as C11 needs for AGY_HOME, same
	// underlying cause: a directory the container creates under a
	// bind-mounted STATE_DIR, owned by its own uid.
	reclaimHostDir(t, stateDir)
	// The container runs as uid 10001 (Dockerfile USER sentinel), distinct
	// from the host uid that owns a Go-created t.TempDir() (default 0700).
	// A bind mount does not remap ownership, so without this the STATE_DIR
	// write-probe fails for uid 10001 regardless of what's actually being
	// tested here — chmod so the case under test (journal readability,
	// not host/container uid mismatch) is what actually gates the result.
	if err := os.Chmod(stateDir, 0o777); err != nil {
		t.Fatal(err)
	}

	// TICK_INTERVAL=abc -> 78
	_, _, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-e", "TICK_INTERVAL=abc", "-e", "STATE_DIR=/state",
		"-v", stateDir+":/state",
		imageTag, "tick", "--once")
	if code != 78 {
		t.Errorf("FAIL C9: TICK_INTERVAL=abc -> code=%d, want 78", code)
	}

	// STATE_DIR unwritable -> 69 (mount a read-only tmpfs-like dir)
	roDir := t.TempDir()
	os.Chmod(roDir, 0o500)
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", roDir+":/state:ro",
		"-e", "STATE_DIR=/state",
		imageTag, "tick", "--once")
	if code != 69 {
		t.Errorf("FAIL C9: unwritable STATE_DIR -> code=%d, want 69", code)
	}

	// neither journal dir readable -> 78 (defaults point at nonexistent
	// /host/journal, /host/journal-volatile: no volume mounted)
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", stateDir+":/state",
		"-e", "STATE_DIR=/state",
		imageTag, "tick", "--once")
	if code != 78 {
		t.Errorf("FAIL C9: no journal mount -> code=%d, want 78", code)
	}

	// --loop --once -> 64
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", imageTag, "tick", "--loop", "--once")
	if code != 64 {
		t.Errorf("FAIL C9: --loop --once -> code=%d, want 64", code)
	}

	// positional argument -> 64
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", imageTag, "tick", "extra-positional")
	if code != 64 {
		t.Errorf("FAIL C9: positional arg -> code=%d, want 64", code)
	}

	t.Log("PASS/FAIL C9 reported per sub-case above")
}

// --- C10: compose security model, via `docker compose config` ---

// sentinelServiceBlock extracts JUST the "sentinel:" service's rendered
// YAML from `docker compose config` output. `docker compose config`
// consistently indents top-level service names by exactly 2 spaces and
// everything belonging to a service by 4+ spaces (verified against a real
// render: "  apprise:", "  mailrise:", "  sentinel:", "  sentinel-net:"
// all sit at column 2; every key under a service sits at column 4+).
// Anchoring on "\n  " (a two-space prefix) as the block end would never
// match inside the block, since the service's own keys sit at four
// spaces — the "end" would be the very next line, and the window
// inspected would be the literal string "sentinel:" with nothing else.
// This anchors on the next line with EXACTLY a 2-space indent (any
// other top-level service or the closing top-level key), which is the
// real boundary.
func sentinelServiceBlock(t *testing.T, text string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^  sentinel:\n`)
	loc := re.FindStringIndex(text)
	if loc == nil {
		t.Fatal("sentinel: service not found in rendered compose config")
	}
	rest := text[loc[1]:]
	endRe := regexp.MustCompile(`(?m)^  \S`) // next 2-space-indented line = next top-level key
	end := endRe.FindStringIndex(rest)
	if end == nil {
		return text[loc[0]:] // sentinel was the last block in the file
	}
	return text[loc[0] : loc[1]+end[0]]
}

func TestContainer_C10_ComposeConfig(t *testing.T) {
	root := repoRoot(t)
	deployDir := filepath.Join(root, "deploy")

	// Skipping outright when deploy/.env (ops-provided, gitignored) is
	// absent would make the ONLY check of the security model disappear
	// on every fresh clone and every CI runner — exactly where you'd
	// most want it run. `docker compose` only needs the `:?`-required
	// variables to render; synthesize a minimal env covering just those
	// instead of depending on ops state.
	// Only the variables docker-compose.yml's sentinel service actually
	// dereferences with `:?` — TELEGRAM_* is never read by this file
	// (R4: the sentinel container gets no TELEGRAM_* variables at all).
	envFile := filepath.Join(t.TempDir(), "ci.env")
	envContent := "JOURNAL_GID=999\n" +
		"AGY_CREDENTIALS_DIR=/tmp\n" +
		"MAILRISE_SMTP_USER=ci\n" +
		"MAILRISE_SMTP_PASS=changeme\n"
	if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(dockerBin(), "compose", "--env-file", envFile, "config")
	cmd.Dir = deployDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("FAIL C10: docker compose config: %v\n%s", err, out)
	}
	text := string(out)
	block := sentinelServiceBlock(t, text)

	// SERVICE-LEVEL read_only, anchored at the 4-space indent `docker
	// compose config` uses for a service's own keys. A bare substring
	// check is not enough here: every individual bind mount under
	// `volumes:` also renders its own "read_only: true" (8+ space
	// indent), so those never disappear even when the SERVICE-level flag
	// is flipped to false. Worse, compose omits a false boolean from the
	// render entirely rather than printing "read_only: false", so the
	// negative case leaves no line to substring-match against at all —
	// only an anchored "is this specific line present" check catches it.
	serviceReadOnlyRe := regexp.MustCompile(`(?m)^    read_only: true$`)
	if !serviceReadOnlyRe.MatchString(block) {
		t.Errorf("FAIL C10: sentinel service block missing the SERVICE-level read_only: true\nblock:\n%s", block)
	}

	checks := []struct {
		name string
		want string
	}{
		{"cap_drop", "- ALL"},
		{"no-new-privileges", "no-new-privileges:true"},
		{"user", "10001:10001"},
	}
	for _, c := range checks {
		if !strings.Contains(block, c.want) {
			t.Errorf("FAIL C10: sentinel service block missing %s (%q)\nblock:\n%s", c.name, c.want, block)
		}
	}

	// R8 C10: "every bind read_only: true". Each `- type: bind` entry
	// renders its own nested "read_only: true" at 8-space indent
	// immediately under it; the one `- type: volume` entry (sentinel-state,
	// deliberately rw) has none. Counting the two against each other
	// catches a bind that lost its :ro without hardcoding the mount list
	// here (which would drift from R4 the moment a mount is added there).
	bindCount := strings.Count(block, "- type: bind")
	nestedReadOnlyRe := regexp.MustCompile(`(?m)^ {8}read_only: true$`)
	nestedReadOnlyCount := len(nestedReadOnlyRe.FindAllString(block, -1))
	if bindCount == 0 {
		t.Error("FAIL C10: sentinel service block has no bind mounts at all — R4 defines several")
	}
	if bindCount != nestedReadOnlyCount {
		t.Errorf("FAIL C10: %d bind mounts but only %d carry read_only: true — every bind must be :ro\nblock:\n%s", bindCount, nestedReadOnlyCount, block)
	}
	forbidden := []string{"privileged:", "cap_add:", "TELEGRAM_", "/config:", "ports:"}
	for _, f := range forbidden {
		if strings.Contains(block, f) {
			t.Errorf("FAIL C10: sentinel service block unexpectedly contains %q\nblock:\n%s", f, block)
		}
	}
	// Every C3 env var must be present in the sentinel service's rendered
	// environment block.
	c3Vars := []string{
		"TICK_INTERVAL", "TICK_WINDOW", "DEEP_WINDOW", "SECTION_TIMEOUT",
		"JOURNAL_MAX_RECORDS", "FACTS_MAX_BYTES", "SERVICES_MAX_BYTES", "STATE_DIR",
		"HOST_JOURNAL_DIR", "HOST_JOURNAL_VOLATILE_DIR", "HOST_PROC", "HOST_ROOT",
		"HOST_RASDAEMON", "SENTINEL_HOSTNAME", "AGY_BIN", "AGY_HOME", "AGY_SECRET_DIR",
		"AGY_PRINT_TIMEOUT", "AGY_HARD_TIMEOUT", "HISTORY_N",
		"HISTORY_KEEP", "DEEP_ENABLED", "DEEP_TIMEOUT", "RAW_ALERT_MAX_PRIORITY",
		"RAW_ALERT_MAX_LINES", "RAW_ALERT_REPEAT_SECONDS", "RAW_ALERT_MARKER_TTL_HOURS",
		"RENOTIFY_ALERT_SEC", "RENOTIFY_WATCH_SEC", "STALE_ALERT_SEC", "HEARTBEAT_HOUR",
		"OUTBOX_MAX", "OUTBOX_SMTP_AFTER", "APPRISE_URL", "APPRISE_KEY",
		"APPRISE_CONFIG_FILE", "NOTIFY_TIMEOUT", "NOTIFY_BODY_MAX", "MAILRISE_HOST",
		"MAILRISE_PORT", "MAILRISE_USER", "MAILRISE_PASS", "SENTINEL_MAIL_FROM",
		"SENTINEL_MAIL_TO", "LOG_LEVEL", "TMPDIR", "TZ",
	}
	// PROMPT_MAX_BYTES is not in the R4 environment block (analyze-only,
	// no compose default listed there) — not in the required set, since
	// asserting it would be checking against a variable the contract's
	// own table never puts there.
	for _, v := range c3Vars {
		if !strings.Contains(block, v+":") {
			t.Errorf("FAIL C10: sentinel environment missing %s", v)
		}
	}
	logPass(t, "PASS C10")
}

// --- C11: SIGTERM shutdown + healthcheck ---

// reclaimHostDir registers a cleanup that chowns everything under hostDir
// back to the invoking host user, via a throwaway root container, so
// Go's own t.TempDir() removal — which runs as that host user — can
// actually delete a tree the sentinel image wrote into as its own
// container uid (10001). On rootful Docker (a real CI runner) that uid
// is a foreign owner on the host side; a subdirectory the container
// creates with a restrictive mode (e.g. seedAgyHome's 0700 AGY_HOME)
// blocks the host user from recursing into it, and t.TempDir()'s
// automatic RemoveAll then fails with "permission denied" — which is
// exactly what surfaced running this suite against real Docker for the
// first time. Rootless podman never shows this: it remaps the
// container's uid to the invoking host user, so nothing it writes is
// ever foreign ownership to begin with.
//
// t.Cleanup funcs run LIFO, so calling this AFTER t.TempDir() — never
// before — makes it run BEFORE TempDir's own removal, leaving the tree
// host-owned by the time Go's cleanup fires. It must be registered even
// when this specific call turns out to have written nothing, since a
// docker image is already required to reach this point (requireImage
// already skipped otherwise) and the chown is a harmless no-op on a
// tree the host user already owns.
func reclaimHostDir(t *testing.T, hostDir string) {
	t.Helper()
	t.Cleanup(func() {
		_, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm",
			"-u", "0:0", "-v", hostDir+":/x",
			"--entrypoint", "chown",
			imageTag, "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/x")
		if code != 0 {
			// Loud, not silent: an ownership reclaim that fails and gets
			// swallowed leaves root/foreign-uid files on the runner after
			// every future run too — this test must not litter the host
			// it ran on.
			t.Errorf("cleanup: reclaiming host ownership of %s failed (code=%d): %s", hostDir, code, errOut)
		}
	})
}

func TestContainer_C11_SIGTERMShutdown(t *testing.T) {
	requireImage(t)
	stateDir := t.TempDir()
	reclaimHostDir(t, stateDir)
	// Container runs as uid 10001 (Dockerfile USER sentinel); a bind mount
	// does not remap host ownership, so the STATE_DIR write-probe needs
	// this to pass preflight at all (same reasoning as C9 above).
	if err := os.Chmod(stateDir, 0o777); err != nil {
		t.Fatal(err)
	}
	hostProc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostProc, "uptime"), []byte("1 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mounted :ro below to match R4's /proc:/host/proc:ro exactly —
	// sentinel writes only under $STATE_DIR and /tmp (R3.9), so
	// nothing here writes to it today, but a test more permissive
	// than the mount it's meant to exercise cannot catch a real
	// regression against that mount: production would fail loudly
	// at the read-only mount itself; a writable test mount would
	// only surface it as a confusing TempDir-cleanup permission
	// error, if it surfaced at all.

	// checkJournalReadable requires a journal that a real journalctl
	// actually reads a record from — a bind-mounted empty temp dir
	// correctly fails preflight, so this test needs the REAL host
	// journal, gid-discovered, same as C2.
	gid, gidOK := hostSystemdJournalGID(t)
	if !gidOK {
		t.Skip("SKIP C11: no real host journal reachable in this environment (checkJournalReadable correctly refuses to start on an empty/synthetic journal dir)")
	}
	selinux := selinuxEnforcingOnDockerHost(t)

	name := "sentinel-c11-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	args := []string{"run", "-d", "--name", name,
		"-e", "STATE_DIR=/state", "-v", stateDir + ":/state",
		"-e", "HOST_JOURNAL_DIR=/host/journal", "-v", "/var/log/journal:/host/journal:ro",
		"-e", "HOST_JOURNAL_VOLATILE_DIR=/host/journal",
		"-e", "HOST_PROC=/hp", "-v", hostProc + ":/hp:ro",
		"-e", "TICK_INTERVAL=60",
		"-e", "MAILRISE_USER=u", "-e", "MAILRISE_PASS=changeme",
		"--group-add", gid,
	}
	if selinux {
		args = append(args, "--security-opt", "label=disable")
	}
	args = append(args, imageTag, "tick", "--loop")
	_, errOut, code := runCmd(t, 20*time.Second, dockerBin(), args...)
	if code != 0 {
		t.Skipf("SKIP C11: could not start container (env limitation): %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", name)

	time.Sleep(3 * time.Second)
	runCmd(t, 5*time.Second, dockerBin(), "kill", "-s", "TERM", name)

	deadline := time.Now().Add(15 * time.Second)
	var exited bool
	for time.Now().Before(deadline) {
		out, _, _ := runCmd(t, 5*time.Second, dockerBin(), "inspect", "-f", "{{.State.Running}}", name)
		if strings.TrimSpace(out) == "false" {
			exited = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !exited {
		t.Fatal("FAIL C11: container did not exit within 15s of SIGTERM")
	}
	out, _, _ := runCmd(t, 5*time.Second, dockerBin(), "inspect", "-f", "{{.State.ExitCode}}", name)
	if strings.TrimSpace(out) != "0" {
		t.Errorf("FAIL C11: exit code = %s, want 0", strings.TrimSpace(out))
	}
	logPass(t, "PASS C11")
}

// --- C12: install-host.sh idempotency ---

func TestContainer_C12_InstallHostIdempotent(t *testing.T) {
	root := repoRoot(t)
	// A throwaway Debian container with apt-get + systemd, network access
	// for real package installs, is what "throwaway rootfs" means here —
	// this test never runs install-host.sh against a real host.
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", filepath.Join(root, "deploy")+":/work:ro",
		"debian:trixie-slim", "sleep", "600")
	if code != 0 {
		t.Skipf("SKIP C12: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// Prep a writable copy + systemd-journal group + apt update, all inside
	// the throwaway container. The gid is deliberately NOT 999 (the real
	// production value, and also what a hardcoded implementation would
	// happen to emit) — a non-999 value is the only way this test can
	// actually distinguish "discovers the gid" from "hardcodes 999";
	// prepping with 999 would pass identically either way.
	const testJournalGID = "7777"
	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
` +
		// The systemd package's own postinst already creates
		// systemd-journal at gid 999 as a side effect of installing it
		// (verified: it does this unconditionally) — so a plain
		// "getent || groupadd" never fires, the group already exists at
		// 999, and this test would silently go back to testing nothing.
		// Force it to the test gid with groupmod, falling back to
		// groupadd only if the group somehow does not exist yet.
		`groupmod -g ` + testJournalGID + ` systemd-journal 2>/dev/null || groupadd -g ` + testJournalGID + ` systemd-journal
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/deploy/.env
chmod +x /root/deploy/install-host.sh`
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12: throwaway container prep failed: %s %s", out, errOut)
	}

	// --dry-run changes nothing.
	before, _, _ := exec_("find /root/deploy -type f -newer /root/deploy/install-host.sh 2>/dev/null | wc -l")
	if out, errOut, code := exec_("cd /root/deploy && ./install-host.sh --dry-run --env-file /root/deploy/.env"); code != 0 {
		t.Errorf("FAIL C12: --dry-run exit code = %d: %s %s", code, out, errOut)
	}
	after, _, _ := exec_("find /root/deploy -type f -newer /root/deploy/install-host.sh 2>/dev/null | wc -l")
	if strings.TrimSpace(before) != strings.TrimSpace(after) {
		t.Errorf("FAIL C12: --dry-run modified files (before=%s after=%s)", before, after)
	}

	// Two consecutive real runs.
	hashFiles := func() string {
		h, _, _ := exec_("sha256sum /etc/msmtprc /etc/smartd.conf /root/deploy/.env 2>/dev/null | sort")
		return h
	}

	out1, errOut1, code1 := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	if code1 != 0 && code1 != 75 {
		// 75 = transient (package/service failure) — acceptable in a
		// throwaway container without a real init system; still assert
		// idempotency of whatever DID converge.
		t.Logf("first run exit=%d (non-fatal for this idempotency check): %s %s", code1, out1, errOut1)
	}
	// This throwaway container has no real PID 1 systemd, so
	// `systemctl enable --now rasdaemon` (step2) fails on EVERY run,
	// always reports rc=75, and never contributes to `changed`. That
	// means "changed=0 on the second run" is satisfied just as well by
	// a run where nothing converged as by a run where everything did —
	// it is not proof of convergence by itself. Assert the actual
	// per-step "already converged" lines instead, for every step this
	// environment CAN converge (everything except step2's service
	// enable, which is a genuine environment limitation here, and
	// step5, skipped because /etc/zfs/zed.d does not exist in this
	// container).
	hash1 := hashFiles()

	// step1 installs msmtp itself (the prep above only installs systemd),
	// so an environment without network reachable from this throwaway
	// container leaves MSMTP_OK=0 on both runs: steps 3/4 then never
	// write anything (compute_mail_status gates them on it), and the
	// "already converged" lines below never appear — not because
	// idempotency broke, but because there is nothing to be idempotent
	// about. That is an environment limitation, the same one the
	// mail-creds-missing case already guards against with this exact
	// check, not a test failure.
	if out, _, code := exec_("dpkg-query -W -f='${Status}' msmtp 2>/dev/null"); code != 0 || !strings.Contains(out, "install ok installed") {
		t.Skipf("SKIP C12: msmtp did not actually install in this environment (no network reachable from the throwaway container?): %s", out)
	}

	out2, errOut2, code2 := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	hash2 := hashFiles()

	if hash1 != hash2 {
		t.Errorf("FAIL C12: sha256 of touched files differs between two real runs:\n1: %s\n2: %s", hash1, hash2)
	}
	for _, want := range []string{
		"step1 packages: already installed",
		"step3 /etc/msmtprc: already converged",
		"step4 /etc/smartd.conf: already converged",
		"step6 JOURNAL_GID: already " + testJournalGID + " in",
	} {
		if !strings.Contains(out2, want) {
			t.Errorf("FAIL C12: second real run did not report convergence for %q: %s (code=%d) %s", want, out2, code2, errOut2)
		}
	}
	// Proves gid DISCOVERY, not a hardcoded 999: the prep above set the
	// systemd-journal group to 7777, and step 6 must have written that
	// exact value, not a constant.
	envContent, _, _ := exec_("cat /root/deploy/.env")
	if !strings.Contains(envContent, "JOURNAL_GID="+testJournalGID) {
		t.Errorf("FAIL C12: /root/deploy/.env does not contain JOURNAL_GID=%s (got: %s) — gid must be DISCOVERED via getent, never hardcoded", testJournalGID, envContent)
	}
	logPass(t, "PASS C12")
}

// TestContainer_C12_CollapsesDuplicateManagedBlocks: everything between
// our own markers is OUR content, never the operator's (unlike the
// pre-existing smartd -m line, which survives as a comment specifically
// because it belongs to them) — so a file that somehow ends up with TWO
// managed blocks (a half-finished run, a restored backup, a merge) is
// our own mess to clean up, not a state a human has to resolve by hand.
// Pre-seeds /etc/smartd.conf with two managed blocks (deliberately
// mismatched from the desired content, so a naive "already converged"
// read of just the first block cannot mask the duplicate) and asserts
// one real run collapses them into a single block and reports having
// done so, and that a second run then reports ordinary convergence with
// an identical sha256 — collapsing is a one-time fixup, not a repeated
// rewrite.
func TestContainer_C12_CollapsesDuplicateManagedBlocks(t *testing.T) {
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12 (duplicate blocks): cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12-dup-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", filepath.Join(root, "deploy")+":/work:ro",
		"debian:trixie-slim", "sleep", "600")
	if code != 0 {
		t.Skipf("SKIP C12 (duplicate blocks): could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	const beginMark = "# >>> agentic-server-supervisor (managed) >>>"
	const endMark = "# <<< agentic-server-supervisor (managed) <<<"

	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
groupadd -g 7777 systemd-journal 2>/dev/null || true
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/deploy/.env
chmod +x /root/deploy/install-host.sh
cat > /etc/smartd.conf <<'SMARTD_EOF'
` + beginMark + `
DEVICESCAN -a -o on -S on -n standby,q -W 4,45,55 -m stale@example.com -M exec /usr/share/smartmontools/smartd-runner
` + endMark + `
` + beginMark + `
DEVICESCAN -a -o on -S on -n standby,q -W 4,45,55 -m stale@example.com -M exec /usr/share/smartmontools/smartd-runner
` + endMark + `
SMARTD_EOF`
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12 (duplicate blocks): throwaway container prep failed: %s %s", out, errOut)
	}

	countBlocks := func() int {
		n, _, _ := exec_(`grep -c '^` + beginMark + `$' /etc/smartd.conf 2>/dev/null || true`)
		v, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			t.Fatalf("could not parse managed-block count %q: %v", n, err)
		}
		return v
	}
	if got := countBlocks(); got != 2 {
		t.Fatalf("setup guard: /etc/smartd.conf must start with 2 managed blocks, got %d", got)
	}

	out1, errOut1, code1 := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	if code1 != 0 && code1 != 75 {
		t.Logf("first run exit=%d (non-fatal for this check): %s %s", code1, out1, errOut1)
	}
	if !strings.Contains(out1, "step4 /etc/smartd.conf: collapsed 2 managed blocks into 1") {
		t.Errorf("FAIL C12 (duplicate blocks): first run did not report the collapse: %s", out1)
	}
	if got := countBlocks(); got != 1 {
		t.Fatalf("FAIL C12 (duplicate blocks): /etc/smartd.conf has %d managed blocks after the collapsing run, want 1", got)
	}
	hash1, _, _ := exec_("sha256sum /etc/smartd.conf")

	out2, _, _ := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	if !strings.Contains(out2, "step4 /etc/smartd.conf: already converged") {
		t.Errorf("FAIL C12 (duplicate blocks): second run did not report convergence: %s", out2)
	}
	if got := countBlocks(); got != 1 {
		t.Errorf("FAIL C12 (duplicate blocks): /etc/smartd.conf has %d managed blocks after the second run, want 1", got)
	}
	hash2, _, _ := exec_("sha256sum /etc/smartd.conf")
	if hash1 != hash2 {
		t.Errorf("FAIL C12 (duplicate blocks): sha256 of /etc/smartd.conf differs between the collapsing run and the next one:\n1: %s\n2: %s", hash1, hash2)
	}
	logPass(t, "PASS C12 (duplicate managed blocks collapse to 1, then stay converged)")
}

// TestContainer_C12_MailCredentialsMissingSkipsAllThreeSteps: msmtp
// package PRESENT, MAILRISE_SMTP_USER/PASS ABSENT. Steps 4 and 5 hand
// their alert mail to msmtp regardless of whether msmtp has anything to
// send with (smartd's `-m` target, ZED_EMAIL_PROG=msmtp) — gating only
// on package presence would let them "converge" while pointing a real
// host's SMART and ZFS alerts at an msmtp with no config file at all,
// which is worse than the pre-existing broken-but-present config this
// whole area of the script exists to fix. All three of steps 3/4/5 must
// refuse to write, and the run must exit 78 (required ops input missing
// from --env-file — permanent until a human edits .env, never 75's
// "safe to re-run").
func TestContainer_C12_MailCredentialsMissingSkipsAllThreeSteps(t *testing.T) {
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12 (mail creds missing): cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12-nocreds-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", filepath.Join(root, "deploy")+":/work:ro",
		"debian:trixie-slim", "sleep", "600")
	if code != 0 {
		t.Skipf("SKIP C12 (mail creds missing): could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// msmtp installed for real (network access, matches the rest of the
	// C12 suite's real-apt-get assumption) — this is the package-PRESENT
	// case, deliberately distinct from MUST 1's package-absent test.
	// .env is intentionally empty: no MAILRISE_SMTP_USER/PASS at all.
	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd msmtp msmtp-mta >/dev/null 2>&1 || true
: > /root/deploy/.env
chmod +x /root/deploy/install-host.sh
mkdir -p /etc/zfs/zed.d`
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12 (mail creds missing): throwaway container prep failed: %s %s", out, errOut)
	}
	if out, _, code := exec_("dpkg-query -W -f='${Status}' msmtp 2>/dev/null"); code != 0 || !strings.Contains(out, "install ok installed") {
		t.Skipf("SKIP C12 (mail creds missing): msmtp did not actually install in this environment: %s", out)
	}

	out1, errOut1, code1 := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	if code1 != 78 {
		t.Errorf("FAIL C12 (mail creds missing): exit code = %d, want 78 (required ops input missing, not 75's transient/retry): %s %s", code1, out1, errOut1)
	}
	for _, want := range []string{
		"step3 /etc/msmtprc: skipped (",
		"step4 /etc/smartd.conf: skipped (",
		"step5 ZED: skipped (",
	} {
		if !strings.Contains(out1, want) {
			t.Errorf("FAIL C12 (mail creds missing): summary missing %q: %s", want, out1)
		}
	}

	if out, _, _ := exec_("test -e /etc/msmtprc && echo exists || echo absent"); strings.TrimSpace(out) != "absent" {
		t.Errorf("FAIL C12 (mail creds missing): /etc/msmtprc must not be written: %s", out)
	}
	if out, _, _ := exec_("test -e /etc/smartd.conf && echo exists || echo absent"); strings.TrimSpace(out) != "absent" {
		t.Errorf("FAIL C12 (mail creds missing): /etc/smartd.conf must not gain a managed block — smartd would then point live alerts at an unconfigured msmtp: %s", out)
	}
	if out, _, _ := exec_("test -e /etc/zfs/zed.d/zed.rc && echo exists || echo absent"); strings.TrimSpace(out) != "absent" {
		t.Errorf("FAIL C12 (mail creds missing): /etc/zfs/zed.d/zed.rc must not gain a managed block — ZED_EMAIL_PROG=msmtp would then be unusable: %s", out)
	}
	logPass(t, "PASS C12 (mail credentials missing skips steps 3, 4 and 5, and exits 78)")
}

// TestContainer_C12_EnvOwnerUnmappedUID: step 6 resolving the .env owner
// by NAME (stat -c %U) breaks silently for a uid with no /etc/passwd
// entry — stat prints the literal string "UNKNOWN", `install -o UNKNOWN`
// fails, and without a checked exit status the step reports "updated"
// while writing nothing. C12's own .env is root-owned throughout (uid 0
// always resolves), so nothing else exercises this path — this test
// chown's it to a uid with deliberately NO passwd entry (1000, present
// nowhere in a fresh debian:trixie-slim's /etc/passwd) and asserts
// JOURNAL_GID still lands.
func TestContainer_C12_EnvOwnerUnmappedUID(t *testing.T) {
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12c: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12c-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", filepath.Join(root, "deploy")+":/work:ro",
		"debian:trixie-slim", "sleep", "300")
	if code != 0 {
		t.Skipf("SKIP C12c: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 60*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
groupmod -g 7777 systemd-journal 2>/dev/null || groupadd -g 7777 systemd-journal
: > /root/deploy/.env
chown 1000:1000 /root/deploy/.env
chmod 600 /root/deploy/.env
chmod +x /root/deploy/install-host.sh
! getent passwd 1000 >/dev/null 2>&1` // the whole point: uid 1000 must have NO passwd entry
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12c: throwaway container prep failed (or uid 1000 unexpectedly has a passwd entry in this base image): %s %s", out, errOut)
	}

	out, errOut, _ = exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	envContent, _, _ := exec_("cat /root/deploy/.env")
	if !strings.Contains(envContent, "JOURNAL_GID=7777") {
		t.Fatalf("FAIL C12c: JOURNAL_GID=7777 missing from .env after install-host.sh against an unmapped-uid-owned .env (stdout=%q stderr=%q .env=%q)", out, errOut, envContent)
	}
	ownerAfter, _, _ := exec_("stat -c '%u:%g' /root/deploy/.env")
	if strings.TrimSpace(ownerAfter) != "1000:1000" {
		t.Errorf("FAIL C12c: .env owner changed to %q, want preserved 1000:1000", strings.TrimSpace(ownerAfter))
	}
	logPass(t, "PASS C12c (JOURNAL_GID written and owner preserved despite no /etc/passwd entry for the owning uid)")
}

// TestContainer_C12_MsmtpDelivery: step3 of install-host.sh must produce
// a /etc/msmtprc that REAL msmtp can actually authenticate and deliver
// through, not merely one containing the string "auth on". Uses a real
// msmtp binary, a real SMTP server requiring AUTH, and the exact config
// file the script writes — asserting the stub actually received an
// authenticated delivery, not that install-host.sh exited 0.
func TestContainer_C12_MsmtpDelivery(t *testing.T) {
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12b: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	stub := newSMTPAuthStub(t)
	defer stub.close()

	// --add-host is set up front (docker run only, not exec) so BOTH
	// aliases below have a real chance to resolve inside this one
	// container: host.containers.internal is Podman's native resolution,
	// host.docker.internal needs the explicit mapping on real Docker.
	// Harmless to pass on either runtime.
	name := "sentinel-c12b-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	runArgs := []string{"run", "-d", "--name", name,
		"--add-host", "host.docker.internal:host-gateway",
		"-v", filepath.Join(root, "deploy") + ":/work:ro",
		"debian:trixie-slim", "sleep", "300"}
	if _, errOut, code := runCmd(t, 60*time.Second, dockerBin(), runArgs...); code != 0 {
		t.Skipf("SKIP C12b: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd msmtp msmtp-mta >/dev/null 2>&1
chmod +x /root/deploy/install-host.sh
printf 'MAILRISE_SMTP_USER=probeuser\nMAILRISE_SMTP_PASS=probepass\n' > /root/deploy/.env`
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12b: throwaway container prep failed (msmtp package unavailable?): %s %s", out, errOut)
	}

	// host.containers.internal (Podman, native) / host.docker.internal
	// (real Docker, needs the --add-host above) both point at the stub
	// listening on this test process's host. Try both; whichever the
	// resolver in THIS environment actually answers is the one that
	// matters.
	for _, alias := range []string{"host.containers.internal", "host.docker.internal"} {
		installCmd := fmt.Sprintf("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env --mailrise-host %s --mailrise-port %d", alias, stub.port())
		if out, errOut, code := exec_(installCmd); code != 0 && code != 75 {
			t.Logf("C12b: install-host.sh with alias %s exited %d (skipping this alias): %s %s", alias, code, out, errOut)
			continue
		}

		sendCmd := `printf 'To: probe@example.com\nSubject: sentinel probe\n\nprobe body\n' | msmtp -a sentinel probe@example.com`
		sendOut, sendErr, sendCode := exec_(sendCmd)
		if sendCode == 0 {
			deliveries, sawAuth, dataText := stub.snapshot()
			if deliveries != 1 {
				t.Fatalf("FAIL C12b: msmtp exited 0 but the stub SMTP server received %d deliveries, want 1 (sendOut=%q sendErr=%q)", deliveries, sendOut, sendErr)
			}
			if !sawAuth {
				t.Fatal("FAIL C12b: msmtp delivered without ever authenticating — mailrise enforces SMTP AUTH unconditionally (R4), so an unauthenticated send proves nothing about the real path")
			}
			if !strings.Contains(dataText, "probe body") {
				t.Fatalf("FAIL C12b: delivered message did not carry the expected body: %q", dataText)
			}
			logPass(t, "PASS C12b (real msmtp, real AUTH, real delivery)")
			return
		}
		t.Logf("C12b: alias %s did not deliver (code=%d): %s %s", alias, sendCode, sendOut, sendErr)
	}
	t.Skip("SKIP C12b: neither host.containers.internal nor host.docker.internal reached the local stub SMTP server from the throwaway container in this environment")
}

// --- SMTP-with-AUTH stub for C12b ---

type smtpAuthStub struct {
	mu         sync.Mutex
	sawAuth    bool
	deliveries int
	dataText   string
	ln         net.Listener
}

func newSMTPAuthStub(t *testing.T) *smtpAuthStub {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpAuthStub{ln: ln}
	go s.serve()
	return s
}

func (s *smtpAuthStub) close() { s.ln.Close() }

func (s *smtpAuthStub) port() int {
	_, portStr, _ := net.SplitHostPort(s.ln.Addr().String())
	p, _ := strconv.Atoi(portStr)
	return p
}

func (s *smtpAuthStub) snapshot() (deliveries int, sawAuth bool, dataText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries, s.sawAuth, s.dataText
}

func (s *smtpAuthStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *smtpAuthStub) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(msg string) { conn.Write([]byte(msg + "\r\n")) }
	write("220 stub.mailrise.local ESMTP")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-stub.mailrise.local")
			write("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(upper, "AUTH PLAIN"), strings.HasPrefix(upper, "AUTH LOGIN"):
			s.mu.Lock()
			s.sawAuth = true
			s.mu.Unlock()
			write("235 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"):
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			write("250 OK")
		case strings.HasPrefix(upper, "DATA"):
			write("354 End data with <CR><LF>.<CR><LF>")
			var data strings.Builder
			for {
				dl, derr := r.ReadString('\n')
				if derr != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				data.WriteString(dl)
			}
			s.mu.Lock()
			s.dataText = data.String()
			s.deliveries++
			s.mu.Unlock()
			write("250 OK: queued")
		case strings.HasPrefix(upper, "QUIT"):
			write("221 Bye")
			return
		default:
			write("500 unrecognized command")
		}
	}
}

// --- C13: workflow lint + metadata shape ---

func TestContainer_C13_WorkflowShape(t *testing.T) {
	root := repoRoot(t)
	wf := filepath.Join(root, ".github", "workflows", "build.yml")
	data, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("FAIL C13: reading %s: %v", wf, err)
	}
	text := string(data)
	for _, want := range []string{
		"type=raw,value=latest",
		"type=sha,format=long",
		"ghcr.io/${{ github.repository }}/sentinel",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("FAIL C13: build.yml missing %q", want)
		}
	}

	if _, err := exec.LookPath("actionlint"); err != nil {
		t.Skip("SKIP C13: actionlint not installed")
	}
	out, errOut, code := runCmd(t, 30*time.Second, "actionlint", wf)
	if code != 0 {
		t.Errorf("FAIL C13: actionlint: %s %s", out, errOut)
	}
	logPass(t, "PASS C13 (workflow shape); pull+run verification is CI-only (needs a published GHCR image, not buildable from a local run)")
}

// --- json sanity helper used by multiple cases indirectly (kept small) ---

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, s)
	}
	return m
}
