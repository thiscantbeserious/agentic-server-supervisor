//go:build container

// Package test holds the container smoke assertions for the sentinel
// image (contracts/runtime.md R8, table C1-C13). Run with:
//
//	go test -tags container -parallel 4 ./test/...
//
// Every TestContainer_* calls t.Parallel: most of a case's wall time is
// spent waiting on a container to start or a package to install, not on
// host CPU, so running them concurrently is a straight win. -parallel
// caps how many run at once (default: GOMAXPROCS); pass it explicitly
// rather than trusting the default, since the heaviest third of this
// suite (the C12 stack cases) each start a full Debian container with
// an apt-get install inside, and enough of those running at once will
// exhaust host memory before it exhausts CPU. CI's container job sets
// -parallel 4 to match its runner's vCPU count.
//
// Every case prints PASS/FAIL/SKIP explicitly (R8: "a SKIP is explicit,
// never a silent pass"). These tests shell out to `docker` (a Podman shim
// is fine locally per CLAUDE.md) and to the repo root's deploy/ artifacts;
// they are NOT run by `go test ./...` (no build tag) or by CI's `test` job
// (R6), only by a dedicated container job/local run on a real Linux host.
//
// Architecture is a property of the process running this suite, not
// something the suite loops over: this file tests the platform it is
// actually running ON (runtime.GOARCH, below), natively, never both,
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
	"io"
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

// arch is this process's own architecture, the runner's. The workflow
// runs this suite once per platform, each on a runner native to that
// platform (no more looping over both architectures inside one process,
// and nothing here to emulate), so runtime.GOARCH IS the platform under
// test. It is also, conveniently, exactly the string Docker's
// --platform linux/<arch> expects ("amd64"/"arm64"), so it needs no
// translation.
var arch = runtime.GOARCH

// imageTag is the image this process builds and every test in it reuses
// (sync.Once via requireImage). Every TestContainer_* below calls
// t.Parallel, but arch and imageTag stay safe as plain package vars:
// both are written once, before any test body runs, and read-only
// afterwards, so concurrent tests only ever read them.
var imageTag = "sentinel:container-test-" + arch

// logPass logs a "PASS ..." line only when nothing in this test has
// already failed. An unconditional t.Log("PASS ...") after a loop of
// non-fatal t.Errorf calls prints PASS right below the FAIL lines it
// contradicts, misleading in a document meant to be a gate record.
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
// architecture-correct static ELF binary, not a #!/bin/sh script. C1's
// coherence check reads agy's own ELF e_machine, since that is the
// value a wrong URL/digest pair would corrupt silently, and a shell
// script has no ELF header at all for that check to read, an empty
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
// URL + sha512. Purely local, never a guessed remote location. Built
// once and cached (sync via fakeAgyMu, same pattern as buildOnce).
//
// The fixture executable is deliberately named "not-agy", NOT "agy"
// (contracts/runtime.md R1): the real vendor tarball contains a single
// ELF executable named "antigravity", the vendor's own installer is
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
	// sidecar file (._not-agy) alongside the real one, extracted on
	// Linux it ALSO carries the executable bit, so the Dockerfile's
	// "exactly one executable" check (correctly) refuses to guess and
	// fails the build with 2 candidates. That is the Dockerfile doing
	// its job; the sidecar file is purely an artifact of building this
	// FIXTURE on macOS and has nothing to do with the real vendor
	// tarball, so it belongs suppressed here, not tolerated there.
	//
	// os.Setenv, not t.Setenv: this runs inside fakeAgyMu's
	// once-only critical section, but the winning caller could be any
	// one of many parallel tests, and t.Setenv panics on a test that
	// has called t.Parallel. The value is harmless left set for the
	// rest of the process (it only affects tar on macOS), so a plain,
	// permanent os.Setenv sidesteps the restriction entirely.
	os.Setenv("COPYFILE_DISABLE", "1")
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
// it returns an error, an unreachable Docker daemon or a sandboxed CI
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

// pick returns val when this process's own arch == want, else "", used
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

// skipUnlessCI distinguishes an honest skip (the subject under test is
// genuinely absent from this host, no hwmon, no rasdaemon, no real
// journal) from a masked failure (a precondition the harness itself
// controls, starting a throwaway container, apt-installing a package,
// did not hold on a run that asked for the check). The first is fine on
// a developer laptop and stays a skip everywhere. The second is fine on
// a laptop too, but not on a CI runner: GitHub Actions sets CI=true with
// nothing to configure, and a runner has a working daemon, network and
// root, so a container-start or package-install failure there is far
// more likely to be a real regression than an environment limitation,
// and every one of C12's five families shares the same "cannot run a
// throwaway container" preamble, so treating it as routine would let a
// single hiccup skip all of install.sh's own coverage and still
// report the job green.
func skipUnlessCI(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// dockerAvailable is a cheap, cached "is the Docker/Podman daemon even
// reachable" probe, checked once per test binary run, kept separate
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
	// Daemon IS reachable past this point, a build failure from here on
	// is either a real Dockerfile/build regression (FAIL) or the
	// synthetic-agy-fixture's own container-to-host networking not
	// working in this sandbox (still a real thing to know, so it FAILs
	// too rather than silently vanishing as a SKIP, the AGY_URL-missing
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
		t.Fatalf("FAIL: %s ELF e_machine = %q, want %q for linux/%s, architecture mismatch", binPath, machine, want, arch)
	}
}

// TestContainer_RealAgyBuild builds the image with real, vendor-published
// agy values, the one check that catches an extraction bug no synthetic
// fixture can, since a synthetic tarball's layout is exactly the layout
// the code was written against. Gated on SENTINEL_REAL_AGY=1 (same gate
// C9/CLAUDE.md already use for real-agy interaction) since it downloads
// the real tarball (~53-56 MB, ~200 MB extracted) from the vendor over
// the network, not something a default `go test ./...` or even the
// default container suite should do.
//
// The values below were independently fetched and sha512-checked on
// 2026-08-19 for agy 1.1.15, one pair per architecture. They are NOT
// Dockerfile/compose defaults, R1 requires AGY_URL_<ARCH>/
// AGY_SHA512_<ARCH> as ops input with no default, on purpose, these are
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
	t.Parallel()
	if os.Getenv("SENTINEL_REAL_AGY") != "1" {
		t.Skip("SKIP: SENTINEL_REAL_AGY != 1, set it to build the image against the REAL agy tarballs over the network (~53-56MB download each, ~200MB extracted each)")
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
		t.Skipf("SKIP: %s unreachable (%v), pinned to agy %s (%s), needs a refresh if the vendor has rotated past it", arch, err, real.version, arch)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Skipf("SKIP: %s returned HTTP %d, pinned to agy %s (%s), needs a refresh if the vendor has rotated past it", arch, resp.StatusCode, real.version, arch)
		}
	}

	root := repoRoot(t)
	tag := "sentinel:real-agy-test-" + arch

	// Both architectures' build-arg pairs are always passed (the unused
	// one just goes unread by this build's TARGETARCH branch in the
	// Dockerfile), this exercises the exact per-arch selection logic a
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
	// context, both need cmd.Dir=root, exactly like buildSentinelImage
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
	// out of the ACTUAL built image, not the synthetic fixture's stub.
	verOut, verErr, verCode := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", "--entrypoint", "agy", tag, "--version")
	if verCode != 0 {
		t.Fatalf("FAIL: agy --version in the real-agy %s image (code=%d): %s %s", arch, verCode, verOut, verErr)
	}
	t.Logf("real agy --version (%s): %s", arch, strings.TrimSpace(verOut))

	sentOut, sentErr, sentCode := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", tag, "--version")
	if sentCode != 0 {
		t.Fatalf("FAIL: sentinel --version in the real-agy %s image (code=%d): %s %s", arch, sentCode, sentOut, sentErr)
	}

	// Coherence check with the REAL binaries, this is the one main
	// named explicitly: "Add agy's ELF to the check, that is the one
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
			t.Logf("real-agy %s image size: %d bytes (%.1f MB), this is what a real host pulls", arch, sizeBytes, float64(sizeBytes)/1e6)
		}
	}
	logPass(t, "PASS real-agy build (%s, version=%s)", arch, real.version)
}

// --- C1: container starts unprivileged ---

func TestContainer_C1_StartsUnprivileged(t *testing.T) {
	t.Parallel()
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
	// regardless of an image's declared platform, platform is
	// manifest metadata, not an execution sandbox, so a stage that
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
	// inspects the architecture of what it downloads, a mismatched
	// AGY_URL_<ARCH>/AGY_SHA512_<ARCH> pairing would corrupt agy
	// silently while sentinel and the image label both stayed correct.
	// This default (synthetic-fixture) path only ever passes the pair
	// matching TARGETARCH (see pick() above), so it catches a MISSING
	// pair loudly but cannot exercise a pair that is present yet
	// mismatched, only TestContainer_RealAgyBuild, which supplies
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
// the Docker/Podman HOST's path, never the local Go test process's own
// filesystem (the C2/C11 lesson: on a Podman Desktop/machine setup this
// test binary runs on macOS while containers run in a separate Linux VM).
//
// Mounts the PARENT of hostPath, never hostPath itself: a rootful daemon
// creates a missing bind-mount SOURCE directory on the host before the
// container starts, so probing a path that does not exist yet would
// itself create it, this test must never mutate the machine it runs
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
// actually "path does not exist", green evidence for an assertion
// never made, so this attaches the REAL R4 mount set (real host paths,
// read-only) so a bind that lost its `:ro` in compose is actually
// caught here.
func TestContainer_C3_ReadOnlySurfaces(t *testing.T) {
	t.Parallel()
	requireImage(t)
	selinux := selinuxEnforcingOnDockerHost(t)

	// {container target, real host source, isDir}. Matches R4's mount
	// list exactly (minus AGY_SECRET_DIR and /state/tmpfs, which are not
	// "ro mount targets", /state and /tmp are the rw exception C3 checks
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

	// /usr/local/bin has no host mount, it's part of the image itself,
	// and read_only:true is what protects it.
	out, _, _ := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /usr/local/bin/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=") || strings.Contains(out, "rc=0") {
		t.Errorf("FAIL C3: write to /usr/local/bin did not fail as expected: %q", out)
	}

	// /state and /tmp are the two rw exceptions, must accept a write
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
// one and journalctl is on PATH, the actual R8 C2 command,
// "journalctl -D /host/journal -n1", against actual binary journal files,
// gated on the actual systemd-journal gid instead of any belief about it.
// "--security-opt label=disable" is added only when SELinux is enforcing
// on the Docker/Podman host itself (common on a Podman Desktop/machine VM)
// , that flag accommodates THIS dev sandbox's mandatory access control on
// bind mounts, is not part of any shipped compose/Dockerfile artifact, and
// a plain-Debian rollout host, with no SELinux, does not need it.
//
// Falls back to a synthetic POSIX-permission probe (a throwaway gid file,
// not a real journal) when no real host journal is reachable, a Linux CI
// runner may have no active journald, so the group_add MECHANISM is still
// exercised even without real journal content. SKIPs loudly, never
// silently, when neither path is set up on this host (C9).
func TestContainer_C2_JournalViaGroupAdd(t *testing.T) {
	t.Parallel()
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
			t.Log("NOTE C2: reading the real journal succeeded even without --group-add on this host, cannot prove the negative direction here, falling through to the positive assertion only")
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
		t.Skip("SKIP C2: this host does not enforce the group permission boundary the way a real Linux target does (probe was readable even without group_add), inconclusive here, needs live-host validation")
	}
	if codeWith != 0 {
		t.Errorf("FAIL C2: --group-add %d still could not read the probe file (code=%d)", testGID, codeWith)
	} else {
		logPass(t, "PASS C2 (synthetic gid fallback, no real host journal reachable in this environment)")
	}
}

// hostSystemdJournalGID discovers the numeric group that owns the
// Docker/Podman host's real /var/log/journal, if one exists, via a
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
	t.Parallel()
	requireImage(t)
	// sensors reads the container's OWN /sys (there is no CLI flag to
	// point it at an arbitrary root), which under Docker/Podman is
	// normally the real host's sysfs shared via the kernel, no /host/sys
	// remapping needed for the standalone binary (that mapping is for
	// collect's own code, contracts/collect.md). On a sandboxed dev VM
	// with no physical sensor chips exposed, `sensors -j` prints "{}" AND
	// exits 1 ("No sensors found!"), that combination is a legitimate
	// "nothing detected" environment limitation, not a Dockerfile defect,
	// so it SKIPs rather than fails; any other non-zero exit is a real
	// failure.
	out, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--entrypoint", "sensors", imageTag, "-j")
	if code != 0 {
		if strings.TrimSpace(out) == "{}" {
			t.Skipf("SKIP C4: sensors -j found no chips in this environment (exit=%d, stderr=%q), ARCHITECTURE §2.6 unverified point, needs a host with real hwmon sensor chips to validate", code, errOut)
		}
		t.Fatalf("FAIL C4: sensors -j exit code = %d: %s %s", code, out, errOut)
	}
	m := mustJSON(t, out)
	if len(m) == 0 {
		t.Skip("SKIP C4: sensors -j returned an empty object, no hwmon sensor chips detected in this environment (ARCHITECTURE §2.6 unverified point; needs a host with real hwmon sensor chips to validate)")
	}
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err != nil || len(entries) == 0 {
		t.Skip("SKIP C4: /host/sys/class/hwmon not readable from the test process itself, cannot cross-check device names here")
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	requireImage(t)
	dir := t.TempDir()
	_, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", dir+":/host/journal:ro",
		"--entrypoint", "journalctl", imageTag, "-D", "/host/journal", "-t", "smartd")
	if code != 0 {
		t.Fatalf("FAIL C8: journalctl -t smartd exit code = %d: %s", code, errOut)
	}
	t.Skip("SKIP C8: no NVMe/smartd fixture data available in this environment (0-hit decode above is not itself the assertion, the contract wants a synthetic 'Killed process' entry to reach the kernel section, which needs a real journal fixture; deferred to internal/collect's own hermetic tests, which already cover kernel-section parsing with testdata/bin)")
}

// --- C6: tmpfs/DNS ok under read_only ---

func TestContainer_C6_TmpfsAndTZ(t *testing.T) {
	t.Parallel()
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
	logPass(t, "PASS C6 (DNS resolution of `apprise` requires the compose network, asserted separately via `docker compose config`, not a bare `docker run`)")
}

// --- C9: sentinel tick exit codes ---

func TestContainer_C9_TickExitCodes(t *testing.T) {
	t.Parallel()
	requireImage(t)
	stateDir := t.TempDir()
	// state.New(cfg) creates active-alerts/history/outbox under
	// STATE_DIR at 0700 (internal/state/state.go) BEFORE any of the
	// config/journal preflight checks these tick invocations exit on
	//, every one of them reaches that far, not just the ones that
	// go on to succeed. Same reclaim as C11 needs for AGY_HOME, same
	// underlying cause: a directory the container creates under a
	// bind-mounted STATE_DIR, owned by its own uid.
	reclaimHostDir(t, stateDir)
	// The container runs as uid 10001 (Dockerfile USER sentinel), distinct
	// from the host uid that owns a Go-created t.TempDir() (default 0700).
	// A bind mount does not remap ownership, so without this the STATE_DIR
	// write-probe fails for uid 10001 regardless of what's actually being
	// tested here, chmod so the case under test (journal readability,
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
// spaces, the "end" would be the very next line, and the window
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
	t.Parallel()
	root := repoRoot(t)
	deployDir := filepath.Join(root, "deploy")

	// Skipping outright when deploy/.env (ops-provided, gitignored) is
	// absent would make the ONLY check of the security model disappear
	// on every fresh clone and every CI runner, exactly where you'd
	// most want it run. `docker compose` only needs the `:?`-required
	// variables to render; synthesize a minimal env covering just those
	// instead of depending on ops state.
	// Only the variables docker-compose.yml's sentinel service actually
	// dereferences with `:?`, TELEGRAM_* is never read by this file
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
	// negative case leaves no line to substring-match against at all,
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
		t.Error("FAIL C10: sentinel service block has no bind mounts at all, R4 defines several")
	}
	if bindCount != nestedReadOnlyCount {
		t.Errorf("FAIL C10: %d bind mounts but only %d carry read_only: true, every bind must be :ro\nblock:\n%s", bindCount, nestedReadOnlyCount, block)
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
	// no compose default listed there), not in the required set, since
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
// Go's own t.TempDir() removal, which runs as that host user, can
// actually delete a tree the sentinel image wrote into as its own
// container uid (10001). On rootful Docker (a real CI runner) that uid
// is a foreign owner on the host side; a subdirectory the container
// creates with a restrictive mode (e.g. seedAgyHome's 0700 AGY_HOME)
// blocks the host user from recursing into it, and t.TempDir()'s
// automatic RemoveAll then fails with "permission denied", which is
// exactly what surfaced running this suite against real Docker for the
// first time. Rootless podman never shows this: it remaps the
// container's uid to the invoking host user, so nothing it writes is
// ever foreign ownership to begin with.
//
// t.Cleanup funcs run LIFO, so calling this AFTER t.TempDir(), never
// before, makes it run BEFORE TempDir's own removal, leaving the tree
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
			// every future run too, this test must not litter the host
			// it ran on.
			t.Errorf("cleanup: reclaiming host ownership of %s failed (code=%d): %s", hostDir, code, errOut)
		}
	})
}

func TestContainer_C11_SIGTERMShutdown(t *testing.T) {
	t.Parallel()
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
	// Mounted :ro below to match R4's /proc:/host/proc:ro exactly,
	// sentinel writes only under $STATE_DIR and /tmp (R3.9), so
	// nothing here writes to it today, but a test more permissive
	// than the mount it's meant to exercise cannot catch a real
	// regression against that mount: production would fail loudly
	// at the read-only mount itself; a writable test mount would
	// only surface it as a confusing TempDir-cleanup permission
	// error, if it surfaced at all.

	// checkJournalReadable requires a journal that a real journalctl
	// actually reads a record from, a bind-mounted empty temp dir
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
		skipUnlessCI(t, "C11: could not start container (env limitation): %s", errOut)
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

// --- C12: install.sh idempotency ---

func TestContainer_C12_InstallHostIdempotent(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	// A throwaway Debian container with apt-get + systemd, network access
	// for real package installs, is what "throwaway rootfs" means here,
	// this test never runs install.sh against a real host.
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", root+":/work:ro",
		"debian:trixie-slim", "sleep", "600")
	if code != 0 {
		skipUnlessCI(t, "C12: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// Prep a writable copy + systemd-journal group + apt update, all inside
	// the throwaway container. The gid is deliberately NOT 999 (the real
	// production value, and also what a hardcoded implementation would
	// happen to emit), a non-999 value is the only way this test can
	// actually distinguish "discovers the gid" from "hardcodes 999";
	// prepping with 999 would pass identically either way.
	const testJournalGID = "7777"
	prep := `set -e
cp -r /work /root/repo
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
` +
		// The systemd package's own postinst already creates
		// systemd-journal at gid 999 as a side effect of installing it
		// (verified: it does this unconditionally), so a plain
		// "getent || groupadd" never fires, the group already exists at
		// 999, and this test would silently go back to testing nothing.
		// Force it to the test gid with groupmod, falling back to
		// groupadd only if the group somehow does not exist yet.
		`groupmod -g ` + testJournalGID + ` systemd-journal 2>/dev/null || groupadd -g ` + testJournalGID + ` systemd-journal
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/repo/.env
chmod +x /root/repo/install.sh`
	if out, errOut, code := exec_(prep); code != 0 {
		skipUnlessCI(t, "C12: throwaway container prep failed: %s %s", out, errOut)
	}

	// --dry-run changes nothing.
	before, _, _ := exec_("find /root/repo -type f -newer /root/repo/install.sh 2>/dev/null | wc -l")
	if out, errOut, code := exec_("cd /root/repo && ./install.sh --dry-run --env-file /root/repo/.env"); code != 0 {
		t.Errorf("FAIL C12: --dry-run exit code = %d: %s %s", code, out, errOut)
	}
	after, _, _ := exec_("find /root/repo -type f -newer /root/repo/install.sh 2>/dev/null | wc -l")
	if strings.TrimSpace(before) != strings.TrimSpace(after) {
		t.Errorf("FAIL C12: --dry-run modified files (before=%s after=%s)", before, after)
	}

	// Two consecutive real runs.
	hashFiles := func() string {
		h, _, _ := exec_("sha256sum /etc/msmtprc /etc/smartd.conf /root/repo/.env 2>/dev/null | sort")
		return h
	}

	out1, errOut1, code1 := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
	if code1 != 0 && code1 != 75 {
		// 75 = transient (package/service failure), acceptable in a
		// throwaway container without a real init system; still assert
		// idempotency of whatever DID converge.
		t.Logf("first run exit=%d (non-fatal for this idempotency check): %s %s", code1, out1, errOut1)
	}
	// This throwaway container has no real PID 1 systemd, so
	// `systemctl enable --now rasdaemon` (step2) fails on EVERY run,
	// always reports rc=75, and never contributes to `changed`. That
	// means "changed=0 on the second run" is satisfied just as well by
	// a run where nothing converged as by a run where everything did,
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
	// "already converged" lines below never appear, not because
	// idempotency broke, but because there is nothing to be idempotent
	// about. That is an environment limitation, the same one the
	// mail-creds-missing case already guards against with this exact
	// check, not a test failure.
	if out, _, code := exec_("dpkg-query -W -f='${Status}' msmtp 2>/dev/null"); code != 0 || !strings.Contains(out, "install ok installed") {
		skipUnlessCI(t, "C12: msmtp did not actually install in this environment (no network reachable from the throwaway container?): %s", out)
	}

	out2, errOut2, code2 := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
	hash2 := hashFiles()

	if hash1 != hash2 {
		t.Errorf("FAIL C12: sha256 of touched files differs between two real runs:\n1: %s\n2: %s", hash1, hash2)
	}
	for _, want := range []string{
		"step1 packages: already installed",
		"step3 /etc/msmtprc: already converged",
		// step4 now asks before touching a running smartd (main's
		// explicit "no flag, ask, defaulting to no"); this throwaway
		// exec has no controlling terminal, so BOTH real runs decline
		// the same way, consistently, that consistency (never writing
		// smartd.conf either time) is itself the idempotency proof
		// this loop exists to make, not "already converged" (which
		// would require a real pty answering yes, covered separately
		// by TestContainer_C12_MonitoringPromptYes/No below).
		"step4 /etc/smartd.conf: skipped, no controlling terminal",
		"step6 JOURNAL_GID: already " + testJournalGID + " in",
	} {
		if !strings.Contains(out2, want) {
			t.Errorf("FAIL C12: second real run did not report convergence for %q: %s (code=%d) %s", want, out2, code2, errOut2)
		}
	}
	// Proves gid DISCOVERY, not a hardcoded 999: the prep above set the
	// systemd-journal group to 7777, and step 6 must have written that
	// exact value, not a constant.
	envContent, _, _ := exec_("cat /root/repo/.env")
	if !strings.Contains(envContent, "JOURNAL_GID="+testJournalGID) {
		t.Errorf("FAIL C12: /root/repo/.env does not contain JOURNAL_GID=%s (got: %s), gid must be DISCOVERED via getent, never hardcoded", testJournalGID, envContent)
	}
	logPass(t, "PASS C12")
}

// runCmdStdin is runCmd with a caller-supplied stdin, needed to pipe
// install.sh's own content into `docker exec -i ... bash -s --`,
// the exact shape of `curl -fsSL URL | sudo bash`: stdin carries the
// script itself, not a place a human answer could come from.
func runCmdStdin(t *testing.T, timeout time.Duration, stdin io.Reader, name string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
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

// runInstallHostPiped pipes install.sh's own content into a
// `docker exec -i` (no -t: no pty, no controlling terminal, this is
// what curl | bash looks like) `bash -s -- ARGS` inside CONTAINER,
// exactly reproducing the target invocation rather than testing the
// script sitting on disk inside the container.
func runInstallHostPiped(t *testing.T, container string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	root := repoRoot(t)
	f, err := os.Open(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatalf("FAIL: opening install.sh: %v", err)
	}
	defer f.Close()
	dockerArgs := append([]string{"exec", "-i", container, "bash", "-s", "--"}, args...)
	return runCmdStdin(t, 90*time.Second, f, dockerBin(), dockerArgs...)
}

// startC12Container is the "throwaway Debian container with apt-get +
// systemd" prep every C12 stack test needs, factored out of
// TestContainer_C12_InstallHostIdempotent's original inline version
// once a fifth test needed the identical setup. Registers its own
// cleanup; the caller does not need a defer.
// c12BaseImage builds, once per run, the throwaway Debian image every stack
// test starts from. It carries exactly what startC12Container used to install
// per test, systemd and curl, and deliberately nothing install.sh itself
// installs: step1 reports "already installed" when its packages are present
// and "installing" when they are not, and tests assert on both branches, so
// pre-seeding those here would quietly rewrite what those tests observe.
//
// Installing per test cost ~90s under emulation and a few seconds natively,
// paid once per test across the whole family. Building once moves that cost
// from per-test to per-suite.
var (
	c12BaseOnce sync.Once
	c12BaseTag  string
	c12BaseErr  error

	c12FullOnce sync.Once
	c12FullTag  string
	c12FullErr  error
)

func c12Base(t *testing.T) string {
	t.Helper()
	c12BaseOnce.Do(func() { c12BaseTag, c12BaseErr = buildC12Base(t, "base", "sentinel-c12base:test") })
	if c12BaseErr != nil {
		skipUnlessCI(t, "C12 (stack): %v", c12BaseErr)
	}
	return c12BaseTag
}

// c12BaseWithPackages is the same host with install.sh's own step1 packages
// already present. Only for tests whose subject is not step1: a test that
// asserts on "installing" versus "already installed" must start from the lean
// base, or it measures a state this image handed it rather than one the script
// produced.
func c12BaseWithPackages(t *testing.T) string {
	t.Helper()
	c12FullOnce.Do(func() { c12FullTag, c12FullErr = buildC12Base(t, "withpackages", "sentinel-c12base:withpackages") })
	if c12FullErr != nil {
		skipUnlessCI(t, "C12 (stack): %v", c12FullErr)
	}
	return c12FullTag
}

func buildC12Base(t *testing.T, target, tag string) (string, error) {
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 15*time.Minute, dockerBin(), "build", "-q",
		"--target", target,
		"-f", filepath.Join(root, "test", "c12base.Dockerfile"),
		"-t", tag, root)
	if code != 0 {
		return "", fmt.Errorf("building the stack-test base image (%s) failed: %s %s", target, out, errOut)
	}
	return tag, nil
}

func startC12Container(t *testing.T, name string) {
	t.Helper()
	startC12ContainerFrom(t, name, c12Base(t))
}

// startC12ContainerFrom is startC12Container with the base image named, for
// the few tests whose subject is not what the base image contains.
func startC12ContainerFrom(t *testing.T, name, base string) {
	t.Helper()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", base, "true")
	if code != 0 {
		skipUnlessCI(t, "C12 (stack): cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", root+":/work:ro",
		base, "sleep", "600")
	if code != 0 {
		skipUnlessCI(t, "C12 (stack): could not start throwaway container: %s", errOut)
	}
	t.Cleanup(func() { runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name) })

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	if out, _, code := exec_("command -v systemctl && command -v curl"); code != 0 {
		skipUnlessCI(t, "C12 (stack): systemd/curl did not actually install in this environment: %s", out)
	}
}

// TestContainer_C12_StackNoTTYRefusesToGuess: `curl -fsSL URL | sudo bash`
// gives the remote process no controlling terminal, and no --stack-dir
// means the script cannot know where to write. This is the single most
// dangerous shape the new stack-creation path can be in, a bug here
// would either hang forever or, worse, guess a directory and start
// writing to it, so the assertion is as blunt as the requirement: exit
// 78, and nothing anywhere on the host changes.
func TestContainer_C12_StackNoTTYRefusesToGuess(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-notty-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 60*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	stdout, stderr, code := runInstallHostPiped(t, name)
	if code != 78 {
		t.Fatalf("FAIL C12 (stack no-tty): exit=%d, want 78 (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "no controlling terminal") {
		t.Errorf("FAIL C12 (stack no-tty): stderr does not explain the real cause: %s", stderr)
	}
	if out, _, _ := exec_("test -e /opt/sentinel && echo EXISTS || echo ABSENT"); !strings.Contains(out, "ABSENT") {
		t.Errorf("FAIL C12 (stack no-tty): /opt/sentinel was created despite exit 78")
	}
	if out, _, _ := exec_("test -e /docker-compose && echo EXISTS || echo ABSENT"); !strings.Contains(out, "ABSENT") {
		t.Errorf("FAIL C12 (stack no-tty): /docker-compose was created despite exit 78")
	}
	logPass(t, "PASS C12 (stack no-tty: refuses to guess, writes nothing)")
}

// TestContainer_C12_StackNoTTYPartialProgress: with --stack-dir given but
// no terminal, the directory and compose file (neither secret) get
// created, but the three secrets are never written, not even as an
// empty assignment, which would be worse than not writing the key at
// all, since an idempotency check reading "KEY=" back would wrongly
// treat it as already set. A second run must not re-do the work it
// already completed, and must still refuse for the same reason.
func TestContainer_C12_StackNoTTYPartialProgress(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-partial-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	stdout1, stderr1, code1 := runInstallHostPiped(t, name, "--stack-dir", "/opt/sentinel")
	if code1 != 78 {
		t.Fatalf("FAIL C12 (stack partial): first run exit=%d, want 78 (stdout=%q stderr=%q)", code1, stdout1, stderr1)
	}
	env1, _, _ := exec_("cat /opt/sentinel/.env 2>&1")
	for _, secretKey := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID", "MAILRISE_SMTP_PASS"} {
		if strings.Contains(env1, secretKey+"=") {
			t.Errorf("FAIL C12 (stack partial): %s written to sentinel.env with no value available, must be omitted entirely, not written empty: %s", secretKey, env1)
		}
	}
	for _, want := range []string{"JOURNAL_GID=", "SENTINEL_TAG=latest", "MAILRISE_SMTP_USER=sentinel"} {
		if !strings.Contains(env1, want) {
			t.Errorf("FAIL C12 (stack partial): sentinel.env missing derived field %q: %s", want, env1)
		}
	}
	if out, _, _ := exec_("test -e /opt/sentinel/mailrise/mailrise.conf && echo EXISTS || echo ABSENT"); !strings.Contains(out, "ABSENT") {
		t.Errorf("FAIL C12 (stack partial): mailrise.conf was written despite missing secrets")
	}
	if out, _, _ := exec_("stat -c '%a' /opt/sentinel/.env"); strings.TrimSpace(out) != "600" {
		t.Errorf("FAIL C12 (stack partial): sentinel.env mode = %s, want 600", strings.TrimSpace(out))
	}

	// Second run: idempotent. Nothing already-set gets rewritten, and the
	// same required-input gap is reported again, not silently dropped
	// (which a broken idempotency check could do by treating "field
	// already visited" as "field already satisfied").
	stdout2, stderr2, code2 := runInstallHostPiped(t, name, "--stack-dir", "/opt/sentinel")
	if code2 != 78 {
		t.Fatalf("FAIL C12 (stack partial): second run exit=%d, want 78 (stdout=%q stderr=%q)", code2, stdout2, stderr2)
	}
	env2, _, _ := exec_("cat /opt/sentinel/.env 2>&1")
	if env1 != env2 {
		t.Errorf("FAIL C12 (stack partial): sentinel.env changed between two no-progress-possible runs:\n1: %s\n2: %s", env1, env2)
	}
	if !strings.Contains(stdout2, "changed=0") {
		t.Errorf("FAIL C12 (stack partial): second run reported new changes when nothing new could converge: %s", stdout2)
	}
	logPass(t, "PASS C12 (stack partial: layout progresses, secrets never written empty, idempotent)")
}

// TestContainer_C12_StackLayoutDetection: the OMV symlink layout
// (sentinel.yml/compose.yml/sentinel.env/.env) is only correct when the
// stack directory's parent is really laid out the way OMV's compose
// plugin lays a shared-folder root out, sibling stacks, each holding
// "<name>.yml" with a "compose.yml" symlink pointing at it, regardless
// of what that root happens to be named on this particular host.
// /docker-compose is one common name for it, never the definition:
// these cases deliberately use OTHER paths to prove detection is
// structural, not a hardcoded string. --dry-run is used throughout:
// this asserts the DECISION, not the write, and needs no secrets.
func TestContainer_C12_StackLayoutDetection(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-layout-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// Plain layout: nothing structurally OMV-shaped anywhere on the host.
	outPlain, _, codePlain := exec_("/work/install.sh --dry-run --stack-dir /opt/sentinel 2>&1")
	if codePlain != 0 {
		t.Fatalf("FAIL C12 (stack layout): plain --dry-run exit=%d: %s", codePlain, outPlain)
	}
	if !strings.Contains(outPlain, "layout: plain") {
		t.Errorf("FAIL C12 (stack layout): expected plain layout with nothing OMV-shaped present, got: %s", outPlain)
	}
	if !strings.Contains(outPlain, "docker-compose.yml") || strings.Contains(outPlain, "sentinel.yml") {
		t.Errorf("FAIL C12 (stack layout): plain layout must write docker-compose.yml directly, not sentinel.yml: %s", outPlain)
	}

	// A directory literally NAMED /docker-compose that has nothing in it
	// is not, by itself, evidence of an OMV compose root, the previous
	// implementation hardcoded exactly this path and would have chosen
	// "omv" here on the name alone. Structural detection must say plain:
	// there is nothing to pattern-match, and no OMV config claiming it.
	exec_("mkdir -p /docker-compose")
	outNamedOnly, _, codeNamedOnly := exec_("/work/install.sh --dry-run --stack-dir /docker-compose/sentinel 2>&1")
	if codeNamedOnly != 0 {
		t.Fatalf("FAIL C12 (stack layout): empty /docker-compose --dry-run exit=%d: %s", codeNamedOnly, outNamedOnly)
	}
	if !strings.Contains(outNamedOnly, "layout: plain") {
		t.Errorf("FAIL C12 (stack layout): an empty directory named /docker-compose must not be treated as an OMV root by name alone: %s", outNamedOnly)
	}

	// A structurally OMV-shaped root at a path that is NOT /docker-compose
	// (a data disk shared folder, the common real-world case), one
	// pre-existing stack with the sentinel.yml + compose.yml symlink
	// shape is enough to identify the root itself as OMV-managed.
	const structuralRoot = "/srv/dev-disk-by-uuid-test1234/docker-compose"
	exec_("mkdir -p " + structuralRoot + "/existingstack && " +
		"printf 'services: {}\\n' > " + structuralRoot + "/existingstack/existingstack.yml && " +
		"ln -sfn existingstack.yml " + structuralRoot + "/existingstack/compose.yml")
	outOMV, _, codeOMV := exec_("/work/install.sh --dry-run --stack-dir " + structuralRoot + "/sentinel 2>&1")
	if codeOMV != 0 {
		t.Fatalf("FAIL C12 (stack layout): structural omv --dry-run exit=%d: %s", codeOMV, outOMV)
	}
	if !strings.Contains(outOMV, "layout: omv") {
		t.Errorf("FAIL C12 (stack layout): expected omv layout under a structurally OMV-shaped root at a non-/docker-compose path, got: %s", outOMV)
	}
	for _, want := range []string{"sentinel.yml", "compose.yml -> sentinel.yml", ".env -> sentinel.env"} {
		if !strings.Contains(outOMV, want) {
			t.Errorf("FAIL C12 (stack layout): omv layout missing %q in dry-run plan: %s", want, outOMV)
		}
	}

	// Auto-detection (no --stack-dir at all) must find the SAME
	// structurally-shaped root and propose "<root>/sentinel" under it,
	// silently, since --dry-run must never prompt.
	outAuto, _, codeAuto := exec_("/work/install.sh --dry-run 2>&1")
	if codeAuto != 0 {
		t.Fatalf("FAIL C12 (stack layout): auto-detect --dry-run exit=%d: %s", codeAuto, outAuto)
	}
	if !strings.Contains(outAuto, "stack directory: "+structuralRoot+"/sentinel") || !strings.Contains(outAuto, "layout: omv") {
		t.Errorf("FAIL C12 (stack layout): auto-detection did not propose the structurally-shaped root %s: %s", structuralRoot, outAuto)
	}

	// A stack directory OUTSIDE any OMV-shaped root gets the plain
	// layout even with one present elsewhere on the host, the
	// directory itself decides, not the host's mere possession of one.
	outElsewhere, _, codeElsewhere := exec_("/work/install.sh --dry-run --stack-dir /opt/sentinel-other 2>&1")
	if codeElsewhere != 0 {
		t.Fatalf("FAIL C12 (stack layout): elsewhere --dry-run exit=%d: %s", codeElsewhere, outElsewhere)
	}
	if !strings.Contains(outElsewhere, "layout: plain") {
		t.Errorf("FAIL C12 (stack layout): --stack-dir /opt/sentinel-other must stay plain even with an OMV root present elsewhere: %s", outElsewhere)
	}

	// A stack directory reached THROUGH A SYMLINK into the structural
	// root must still be recognized as omv: dirname is purely lexical,
	// so without resolving the path first, /srv/compose2/sentinel (where
	// /srv/compose2 -> the structural root) would be misclassified
	// plain, the files would land in the right place on disk while
	// OMV's compose plugin, which enumerates by real path, never sees
	// the stack at all.
	exec_("ln -sfn " + structuralRoot + " /srv/compose2")
	outSymlinked, _, codeSymlinked := exec_("/work/install.sh --dry-run --stack-dir /srv/compose2/sentinel 2>&1")
	if codeSymlinked != 0 {
		t.Fatalf("FAIL C12 (stack layout): symlinked --dry-run exit=%d: %s", codeSymlinked, outSymlinked)
	}
	if !strings.Contains(outSymlinked, "layout: omv") {
		t.Errorf("FAIL C12 (stack layout): --stack-dir /srv/compose2/sentinel (symlink to the structural root) must resolve to omv layout, got: %s", outSymlinked)
	}
	if !strings.Contains(outSymlinked, "sentinel.yml") {
		t.Errorf("FAIL C12 (stack layout): symlinked omv path must still get sentinel.yml, not docker-compose.yml: %s", outSymlinked)
	}

	// --stack-dir pointing at the detected OMV compose ROOT itself (not
	// a stack directory inside it) must be refused, not silently
	// treated as plain, that would drop a stray docker-compose.yml
	// into the directory where OMV enumerates every stack. Checked
	// against both the literal path and a symlink resolving to it.
	outRoot, _, codeRoot := exec_("/work/install.sh --dry-run --stack-dir " + structuralRoot + " 2>&1")
	if codeRoot != 64 {
		t.Errorf("FAIL C12 (stack layout): --stack-dir %s (the detected root) exit=%d, want 64: %s", structuralRoot, codeRoot, outRoot)
	}
	outRootSymlinked, _, codeRootSymlinked := exec_("/work/install.sh --dry-run --stack-dir /srv/compose2 2>&1")
	if codeRootSymlinked != 64 {
		t.Errorf("FAIL C12 (stack layout): --stack-dir /srv/compose2 (symlink to the detected root) exit=%d, want 64: %s", codeRootSymlinked, outRootSymlinked)
	}

	// The earlier empty /docker-compose directory is NOT a detected root
	// (nothing structural, no OMV config claiming it), pointing
	// --stack-dir directly at it must NOT be refused. This is the
	// positive proof that refusal now follows the detected shape, not
	// the literal string "/docker-compose".
	outNamedOnlyRoot, _, codeNamedOnlyRoot := exec_("/work/install.sh --dry-run --stack-dir /docker-compose 2>&1")
	if codeNamedOnlyRoot != 0 {
		t.Errorf("FAIL C12 (stack layout): --stack-dir /docker-compose (empty, not a detected root) must proceed as plain, not be refused: exit=%d: %s", codeNamedOnlyRoot, outNamedOnlyRoot)
	}
	logPass(t, "PASS C12 (stack layout: omv detected structurally, not by hardcoded path, symlinks resolved, detected root itself refused)")
}

// TestContainer_C12_StackLayoutDetectionSymlinkTargetShapes: a real
// OpenMediaVault host writes compose.yml as an ABSOLUTE symlink, not
// the relative one every other fixture in this file uses:
//
//	lrwxrwxrwx 1 root root 57 compose.yml -> /docker-compose/restic-rest-server/restic-rest-server.yml
//
// compose_root_looks_omv's original equality check
// (readlink(compose.yml) == "name.yml") only ever matched a bare
// relative target, so it silently found nothing on a real host and
// fell back to the plain default. Covers the four shapes the fix must
// tell apart: plain relative, absolute (the real one), dotted relative,
// and a same-basename target that resolves to a DIFFERENT directory,
// which the check exists to reject.
func TestContainer_C12_StackLayoutDetectionSymlinkTargetShapes(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-layout-symlink-shapes-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	checkOMV := func(label, root, linkCmd string) {
		exec_("mkdir -p " + root + "/existingstack && " +
			"printf 'services: {}\\n' > " + root + "/existingstack/existingstack.yml && " +
			linkCmd)
		out, _, code := exec_("/work/install.sh --dry-run --stack-dir " + root + "/sentinel 2>&1")
		if code != 0 {
			t.Fatalf("FAIL C12 (symlink shapes, %s): --dry-run exit=%d: %s", label, code, out)
		}
		if !strings.Contains(out, "layout: omv") {
			t.Errorf("FAIL C12 (symlink shapes, %s): expected omv layout, got: %s", label, out)
		}
	}

	// Plain relative target, "name.yml": the shape every other fixture
	// in this file already uses, kept here so all four shapes are
	// asserted in one place.
	checkOMV("relative",
		"/srv/dev-disk-by-uuid-relative/docker-compose",
		"ln -sfn existingstack.yml /srv/dev-disk-by-uuid-relative/docker-compose/existingstack/compose.yml")

	// Absolute target, matching a real OMV host's own output exactly.
	// This is the shape that was broken: readlink returns the full
	// path, which never equals the bare "name.yml" the old check
	// compared against.
	checkOMV("absolute",
		"/srv/dev-disk-by-uuid-absolute/docker-compose",
		"ln -sfn /srv/dev-disk-by-uuid-absolute/docker-compose/existingstack/existingstack.yml "+
			"/srv/dev-disk-by-uuid-absolute/docker-compose/existingstack/compose.yml")

	// Dotted relative target, "./name.yml": still relative, but not
	// byte-identical to the bare "name.yml" the old check compared
	// against either.
	checkOMV("dotted relative",
		"/srv/dev-disk-by-uuid-dotted/docker-compose",
		"ln -sfn ./existingstack.yml /srv/dev-disk-by-uuid-dotted/docker-compose/existingstack/compose.yml")

	// Same basename, wrong directory: compose.yml points at a file
	// named existingstack.yml, but in an entirely different directory
	// than the one it lives in. A real existingstack.yml also exists
	// right next to it, so only the resolved-directory comparison can
	// catch this, not the "does <base>.yml exist here" check alone.
	// This is the case the whole detector exists to exclude, and the
	// fix must not accept it just because it now compares basenames.
	const wrongDirRoot = "/srv/dev-disk-by-uuid-wrongdir/docker-compose"
	const elsewhere = "/srv/dev-disk-by-uuid-wrongdir/elsewhere"
	exec_("mkdir -p " + wrongDirRoot + "/existingstack && " +
		"printf 'services: {}\\n' > " + wrongDirRoot + "/existingstack/existingstack.yml && " +
		"mkdir -p " + elsewhere + " && " +
		"printf 'services: {}\\n' > " + elsewhere + "/existingstack.yml && " +
		"ln -sfn " + elsewhere + "/existingstack.yml " + wrongDirRoot + "/existingstack/compose.yml")
	outWrong, _, codeWrong := exec_("/work/install.sh --dry-run --stack-dir " + wrongDirRoot + "/sentinel 2>&1")
	if codeWrong != 0 {
		t.Fatalf("FAIL C12 (symlink shapes, wrong directory): --dry-run exit=%d: %s", codeWrong, outWrong)
	}
	if !strings.Contains(outWrong, "layout: plain") {
		t.Errorf("FAIL C12 (symlink shapes, wrong directory): a compose.yml pointing at a same-named file in a different directory must not be treated as an OMV stack, got: %s", outWrong)
	}

	logPass(t, "PASS C12 (symlink shapes: relative, absolute and dotted targets all detected, cross-directory target rejected)")
}

// TestContainer_C12_StackLayoutConfigFallback: a freshly enabled OMV
// compose plugin with zero stacks created yet has nothing on disk for
// structural detection to pattern-match, the empty-root case R5 calls
// out explicitly. omv-confdbadm is the fallback for exactly that case;
// stubbed here (this container has no real OMV installation) to prove
// the wiring, not the real command's output shape, which was not
// available to verify against a live host for this change.
func TestContainer_C12_StackLayoutConfigFallback(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-layout-cfgfallback-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	const emptyRoot = "/srv/dev-disk-by-uuid-fresh/docker-compose"
	exec_("mkdir -p " + emptyRoot)
	stub := `cat > /usr/local/bin/omv-confdbadm <<'STUB'
#!/bin/sh
for a in "$@"; do id="$a"; done
if [ "$id" = "conf.service.compose" ]; then
  printf '{"sharedfolderref":"11111111-1111-1111-1111-111111111111"}\n'
elif [ "$id" = "conf.system.sharedfolder" ]; then
  printf '{"uuid":"11111111-1111-1111-1111-111111111111","mntentref":"22222222-2222-2222-2222-222222222222","reldirpath":"docker-compose/","privileges":{"privilege":[]}}\n'
elif [ "$id" = "conf.system.filesystem.mountpoint" ]; then
  printf '{"uuid":"22222222-2222-2222-2222-222222222222","dir":"/srv/dev-disk-by-uuid-fresh","type":"ext4"}\n'
fi
STUB
chmod +x /usr/local/bin/omv-confdbadm`
	if out, errOut, code := exec_(stub); code != 0 {
		t.Fatalf("FAIL C12 (stack layout config fallback): stub setup failed: %s %s", out, errOut)
	}

	// The empty root has no siblings to pattern-match, so structural
	// detection alone is inconclusive, the stubbed omv-confdbadm answer
	// must be what settles it as omv.
	out, _, code := exec_("/work/install.sh --dry-run --stack-dir " + emptyRoot + "/sentinel 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (stack layout config fallback): exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "layout: omv") {
		t.Errorf("FAIL C12 (stack layout config fallback): an empty root confirmed by omv-confdbadm must still be detected as omv: %s", out)
	}

	// The empty root itself must be refused too, via the same
	// config-based fallback (structural detection cannot see it either).
	outRoot, _, codeRoot := exec_("/work/install.sh --dry-run --stack-dir " + emptyRoot + " 2>&1")
	if codeRoot != 64 {
		t.Errorf("FAIL C12 (stack layout config fallback): --stack-dir %s (the config-confirmed empty root) exit=%d, want 64: %s", emptyRoot, codeRoot, outRoot)
	}

	// Auto-detection with no --stack-dir must also find it via the stub
	// and propose "<root>/sentinel" under it.
	outAuto, _, codeAuto := exec_("/work/install.sh --dry-run 2>&1")
	if codeAuto != 0 {
		t.Fatalf("FAIL C12 (stack layout config fallback): auto-detect exit=%d: %s", codeAuto, outAuto)
	}
	if !strings.Contains(outAuto, "stack directory: "+emptyRoot+"/sentinel") || !strings.Contains(outAuto, "layout: omv") {
		t.Errorf("FAIL C12 (stack layout config fallback): auto-detection did not propose the config-confirmed empty root: %s", outAuto)
	}
	logPass(t, "PASS C12 (stack layout config fallback: omv-confdbadm settles the empty-root case)")
}

// TestContainer_C12_StackDirCandidatesAmbiguous: a host can genuinely
// have MORE THAN ONE directory structurally shaped like an OMV compose
// root (a leftover from a migrated install, a second shared folder),
// auto-detection must never guess between them. Asserts the candidate
// LIST itself (both real paths named, not just whichever one happens to
// get picked), per the rule this project keeps re-learning: a test that
// only checks the chosen path proves nothing about whether the scan
// found the right candidates.
//
// rootUUID and rootOpt are chosen so that NEITHER is a substring of the
// other (reviewer-caught defect: an earlier version used "/docker-compose"
// and ".../docker-compose", so both strings.Contains checks passed
// whether or not "/docker-compose" itself was ever scanned, confirmed
// by mutating candidate_compose_roots to glob "/docker-compose-x"
// instead and watching every assertion still hold). Assertions below
// anchor on the numbered-menu format ") <path> (" specifically, not a
// bare substring match, so a differently-named directory cannot satisfy
// them by accident either.
func TestContainer_C12_StackDirCandidatesAmbiguous(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-candidates-ambiguous-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// candidate_compose_roots scans /srv/dev-disk-by-uuid-*/* BEFORE
	// /opt/* (fixed pattern order), so rootUUID is always candidate 1
	// and rootOpt always candidate 2, asserted explicitly below rather
	// than assumed, so a reordering of candidate_compose_roots would
	// fail this test loudly instead of silently picking the wrong one.
	const rootUUID = "/srv/dev-disk-by-uuid-second/docker-compose"
	const rootOpt = "/opt/manual-compose-root"
	for _, root := range []string{rootUUID, rootOpt} {
		exec_("mkdir -p " + root + "/existingstack && " +
			"printf 'services: {}\\n' > " + root + "/existingstack/existingstack.yml && " +
			"ln -sfn existingstack.yml " + root + "/existingstack/compose.yml")
	}
	menuLine := func(path string) string { return ") " + path + " (" }

	// --dry-run: never prompts, must report the ambiguity AND name both
	// candidates in the numbered menu, a preview that silently picks
	// one, or that hides the second candidate, is worse than one that
	// shows the ambiguity.
	outDry, _, codeDry := exec_("/work/install.sh --dry-run 2>&1")
	if codeDry != 0 {
		t.Fatalf("FAIL C12 (stack dir candidates ambiguous): --dry-run exit=%d: %s", codeDry, outDry)
	}
	for _, want := range []string{menuLine(rootUUID), menuLine(rootOpt)} {
		if !strings.Contains(outDry, want) {
			t.Errorf("FAIL C12 (stack dir candidates ambiguous): --dry-run menu missing %q: %s", want, outDry)
		}
	}
	if !strings.Contains(outDry, "  1) "+rootUUID) || !strings.Contains(outDry, "  2) "+rootOpt) {
		t.Errorf("FAIL C12 (stack dir candidates ambiguous): --dry-run menu numbering not as expected (1=%s, 2=%s): %s", rootUUID, rootOpt, outDry)
	}
	if !strings.Contains(outDry, "2 possible compose roots") {
		t.Errorf("FAIL C12 (stack dir candidates ambiguous): --dry-run does not report the candidate count: %s", outDry)
	}
	if !strings.Contains(outDry, "ambiguous") && !strings.Contains(outDry, "multiple possible") {
		t.Errorf("FAIL C12 (stack dir candidates ambiguous): --dry-run does not flag the ambiguity in its output: %s", outDry)
	}

	// DEFECT 4 (reviewer, house defect repeated: state computed past an
	// early return, same shape as RENDER_COLLAPSED): with no stack
	// directory resolved, step0b_secrets and step6 must not preview a
	// plan against the unrelated ./.env default, that reads as "here
	// is what I will do" when the correct message is "I stopped".
	// "in ./.env" legitimately appears in steps 3-5's skip reasons
	// (compute_mail_status genuinely reads the ./.env default and
	// reports it has no credentials, honest, and orthogonal to
	// step0b_secrets/step6), the specific phrasings that would mean a
	// PLAN was previewed against it are what must never appear.
	for _, mustNotAppear := range []string{
		"would write 11 field(s) to ./.env",
		"JOURNAL_GID: would set to",
	} {
		if strings.Contains(outDry, mustNotAppear) {
			t.Errorf("FAIL C12 (stack dir candidates ambiguous): --dry-run previews a plan against the stale ./.env default (%q found) despite refusing to resolve a stack directory: %s", mustNotAppear, outDry)
		}
	}
	for _, wantSkip := range []string{
		"stack env: skipped",
		"step6 JOURNAL_GID: skipped",
	} {
		if !strings.Contains(outDry, wantSkip) {
			t.Errorf("FAIL C12 (stack dir candidates ambiguous): --dry-run does not report %q when no stack directory was resolved: %s", wantSkip, outDry)
		}
	}

	// A real run with no controlling terminal and no --stack-dir must
	// refuse outright (exit 78, the same code the zero-terminal/
	// zero-candidate case already uses) and name both candidates in the
	// numbered refusal, never guess which one the operator meant.
	outReal, errOutReal, codeReal := exec_("/work/install.sh 2>&1")
	if codeReal != 78 {
		t.Fatalf("FAIL C12 (stack dir candidates ambiguous): real run exit=%d, want 78: %s %s", codeReal, outReal, errOutReal)
	}
	for _, want := range []string{menuLine(rootUUID), menuLine(rootOpt)} {
		if !strings.Contains(outReal, want) {
			t.Errorf("FAIL C12 (stack dir candidates ambiguous): real-run refusal missing %q: %s", want, outReal)
		}
	}
	if out, _, _ := exec_("test -e " + rootUUID + "/sentinel && echo EXISTS || echo ABSENT"); !strings.Contains(out, "ABSENT") {
		t.Errorf("FAIL C12 (stack dir candidates ambiguous): a stack directory was created despite refusing to choose")
	}

	// Interactive: a real terminal choosing "2" must land under rootOpt
	// SPECIFICALLY (candidate 2 by the fixed scan order asserted above),
	// not silently default to candidate 1, proves the numbered choice
	// is wired to the right entry, not just that pressing a key unblocks
	// the prompt.
	if out, _, code := exec_("command -v script"); code != 0 {
		skipUnlessCI(t, "C12 (stack dir candidates ambiguous): the `script` utility is not available in this environment: %s", out)
	}
	// A real run, not --check/--dry-run: --check/--dry-run never prompt
	// at all (they take the "report and stop" branch above), so only a
	// real run reaches the interactive numbered choice. "2" picks the
	// directory, then three blank answers (Enter with nothing typed) at
	// the token/chat id/mailrise password prompts that inevitably
	// follow, a bare EOF there left `read` blocking against the pty
	// instead of returning, hanging the run past this test's own
	// timeout, so every remaining prompt gets an explicit empty
	// answer instead. Enter-with-nothing at a real prompt is itself
	// "still required" (require_secret's own fail-closed rule), landing
	// on exit 78, irrelevant here; what matters is which stack
	// directory got resolved BEFORE any of that, printed unconditionally
	// as soon as step0a_layout settles it.
	driveCmd := `printf '2\n\n\n\n' | script -qec "bash -s -- < /work/install.sh" /tmp/script-ambig.log`
	outI, errOutI, codeI := exec_(driveCmd)
	if codeI != 78 {
		t.Fatalf("FAIL C12 (stack dir candidates ambiguous, interactive): exit=%d, want 78 (secrets prompts hit EOF after the directory choice): %s %s", codeI, outI, errOutI)
	}
	if !strings.Contains(outI, "stack directory: "+rootOpt+"/sentinel") {
		t.Errorf("FAIL C12 (stack dir candidates ambiguous, interactive): choosing \"2\" did not select %s: %s", rootOpt, outI)
	}
	if strings.Contains(outI, "stack directory: "+rootUUID+"/sentinel") {
		t.Errorf("FAIL C12 (stack dir candidates ambiguous, interactive): choosing \"2\" wrongly selected candidate 1 (%s): %s", rootUUID, outI)
	}
	logPass(t, "PASS C12 (stack dir candidates ambiguous: never guessed, both candidates named in the numbered menu, numbered choice wired correctly)")
}

// TestContainer_C12_StackDirCandidatesNone: nothing structurally
// OMV-shaped anywhere on the host, and no omv-confdbadm, the
// conventional /opt/sentinel default is proposed exactly as it was
// before OMV detection existed at all, with no ambiguity language and
// no candidate list (there is nothing to list).
func TestContainer_C12_StackDirCandidatesNone(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-candidates-none-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	out, _, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (stack dir candidates none): --dry-run exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "stack directory: /opt/sentinel") {
		t.Errorf("FAIL C12 (stack dir candidates none): expected the conventional /opt/sentinel default with nothing OMV-shaped present, got: %s", out)
	}
	// Zero candidates must say so, not fall to the plain default
	// silently: that reads as a deliberate choice instead of "the
	// scan searched and found nothing", the exact ambiguity the
	// 1-candidate and 2+-candidate branches already report on stderr.
	if !strings.Contains(out, "no compose root detected") {
		t.Errorf("FAIL C12 (stack dir candidates none): zero detected candidates must be reported, not silently defaulted: %s", out)
	}
	for _, mustNotAppear := range []string{"ambiguous", "multiple possible", "detected a possible"} {
		if strings.Contains(out, mustNotAppear) {
			t.Errorf("FAIL C12 (stack dir candidates none): unexpected detection language %q with zero candidates: %s", mustNotAppear, out)
		}
	}
	logPass(t, "PASS C12 (stack dir candidates none: conventional default, no phantom detection)")
}

// dockerComposeLsStub installs a fake /usr/local/bin/docker that reports
// `docker info` and `docker compose version` as ready (so docker_preflight's
// own DOCKER_OK/COMPOSE_OK never confound what these tests are actually
// checking, the separate `docker compose ls --all --format json` primary
// detection signal) and answers `docker compose ls --all --format json` with
// exactly `lsBody`, exiting `lsExit`. Letting each test control the ls output
// verbatim is what lets the "malformed"/"empty"/"daemon rejects the command"
// cases below be expressed as plain fixture data instead of new stub logic
// each time.
func dockerComposeLsStub(lsBody string, lsExit int) string {
	return fmt.Sprintf(`cat > /usr/local/bin/docker <<'DOCKEREOF'
#!/bin/sh
if [ "$1" = "info" ]; then exit 0; fi
if [ "$1" = "compose" ] && [ "$2" = "version" ]; then exit 0; fi
if [ "$1" = "compose" ] && [ "$2" = "ls" ]; then
  cat <<'JSONEOF'
%s
JSONEOF
  exit %d
fi
exit 1
DOCKEREOF
chmod +x /usr/local/bin/docker`, lsBody, lsExit)
}

// TestContainer_C12_DockerSignalPrimaryDetectsRunningProjects: the PRIMARY
// signal (R5), `docker compose ls --all --format json` reporting existing
// projects, must be enough on its own to detect a compose root, with no
// OMV-style structural shape and no omv-confdbadm anywhere in the picture.
// The candidate's PARENT directory (not either project's own directory) is
// what gets proposed, and the proposal names WHY: how many compose projects
// docker already found there.
func TestContainer_C12_DockerSignalPrimaryDetectsRunningProjects(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-docker-signal-primary-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	exec_("mkdir -p /opt/dockerstacks/proj1 /opt/dockerstacks/proj2")
	lsBody := `[{"Name":"proj1","Status":"running(1)","ConfigFiles":"/opt/dockerstacks/proj1/proj1.yml"},` +
		`{"Name":"proj2","Status":"running(1)","ConfigFiles":"/opt/dockerstacks/proj2/proj2.yml"}]`
	if out, errOut, code := exec_(dockerComposeLsStub(lsBody, 0)); code != 0 {
		t.Fatalf("FAIL C12 (docker signal primary): could not install docker stub: %s %s", out, errOut)
	}

	out, errOut, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (docker signal primary): --dry-run exit=%d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "stack directory: /opt/dockerstacks/sentinel") {
		t.Errorf("FAIL C12 (docker signal primary): expected the projects' PARENT directory to be proposed as the stack root, got: %s", out)
	}
	if !strings.Contains(out, "detected a possible compose root at /opt/dockerstacks (2 compose projects already here)") {
		t.Errorf("FAIL C12 (docker signal primary): expected the docker-count provenance in the proposal, got: %s", out)
	}
	logPass(t, "PASS C12 (docker signal primary: two running projects propose their shared parent, with provenance)")
}

// TestContainer_C12_DockerSignalDegradesQuietly covers every shape R5
// requires the docker signal to treat as normal, not as an error: output
// that is not the documented JSON shape at all, a syntactically valid but
// empty project list, and the command itself failing (an unreachable
// daemon or an unsupported `compose ls` both look like this from here).
// None of these may block the run or introduce a new failure mode, each
// must fall through exactly as if docker were entirely absent.
func TestContainer_C12_DockerSignalDegradesQuietly(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-docker-signal-degrade-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	cases := []struct {
		name   string
		lsBody string
		lsExit int
	}{
		{"not JSON at all", "Cannot connect to the Docker daemon", 0},
		{"empty project list", "[]", 0},
		{"compose ls itself fails", `[{"Name":"x"}]`, 1},
	}
	for _, c := range cases {
		if out, errOut, code := exec_(dockerComposeLsStub(c.lsBody, c.lsExit)); code != 0 {
			t.Fatalf("FAIL C12 (docker signal degrades quietly, %s): could not install docker stub: %s %s", c.name, out, errOut)
		}
		out, errOut, code := exec_("/work/install.sh --dry-run 2>&1")
		if code != 0 {
			t.Fatalf("FAIL C12 (docker signal degrades quietly, %s): --dry-run exit=%d, want 0 (must degrade, never fail): %s %s", c.name, code, out, errOut)
		}
		if !strings.Contains(out, "no compose root detected, using the conventional default") {
			t.Errorf("FAIL C12 (docker signal degrades quietly, %s): expected the zero-candidate fallback with nothing else on the host to detect, got: %s", c.name, out)
		}
		if !strings.Contains(out, "stack directory: /opt/sentinel") {
			t.Errorf("FAIL C12 (docker signal degrades quietly, %s): expected the conventional default, got: %s", c.name, out)
		}
	}
	logPass(t, "PASS C12 (docker signal degrades quietly: malformed output, empty list, and a failing command are all normal, never errors)")
}

// TestContainer_C12_DockerSignalIgnoresGoneWorkingDir: a project docker
// still lists whose compose-file directory no longer exists on disk (the
// project was removed by hand, or lives on an unmounted volume) must be
// skipped, not surfaced as a candidate pointing at a directory that isn't
// there, "a project whose working directory no longer exists" is named
// explicitly in R5 as a normal case, not an error.
func TestContainer_C12_DockerSignalIgnoresGoneWorkingDir(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-docker-signal-gone-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// /opt/ghostproj is deliberately never created.
	lsBody := `[{"Name":"ghost","Status":"running(1)","ConfigFiles":"/opt/ghostproj/ghost.yml"}]`
	if out, errOut, code := exec_(dockerComposeLsStub(lsBody, 0)); code != 0 {
		t.Fatalf("FAIL C12 (docker signal gone working dir): could not install docker stub: %s %s", out, errOut)
	}

	out, errOut, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (docker signal gone working dir): --dry-run exit=%d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "no compose root detected, using the conventional default") {
		t.Errorf("FAIL C12 (docker signal gone working dir): a project pointing at a nonexistent directory must not produce a candidate: %s", out)
	}
	if strings.Contains(out, "ghostproj") {
		t.Errorf("FAIL C12 (docker signal gone working dir): the nonexistent project directory leaked into the output: %s", out)
	}
	logPass(t, "PASS C12 (docker signal ignores a project whose working directory is gone)")
}

// TestContainer_C12_DockerSignalDedupAndOutranksStructural: when the SAME
// resolved root is found by both the docker signal and the structural
// scan, R5 requires ONE candidate, not two, and the higher-priority
// signal's reason, docker is primary precisely because it is a fact
// about the host rather than an inference from directory shape, so its
// provenance is what the operator should see, not the structural
// scan's stack count.
func TestContainer_C12_DockerSignalDedupAndOutranksStructural(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-docker-signal-dedup-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	const root = "/opt/manualroot"
	exec_("mkdir -p " + root + "/existingstack && " +
		"printf 'services: {}\\n' > " + root + "/existingstack/existingstack.yml && " +
		"ln -sfn existingstack.yml " + root + "/existingstack/compose.yml && " +
		"mkdir -p " + root + "/dockerproj")
	lsBody := `[{"Name":"dockerproj","Status":"running(1)","ConfigFiles":"` + root + `/dockerproj/dockerproj.yml"}]`
	if out, errOut, code := exec_(dockerComposeLsStub(lsBody, 0)); code != 0 {
		t.Fatalf("FAIL C12 (docker signal dedup): could not install docker stub: %s %s", out, errOut)
	}

	out, errOut, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (docker signal dedup): --dry-run exit=%d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "detected a possible compose root at "+root+" (1 compose project already here)") {
		t.Errorf("FAIL C12 (docker signal dedup): expected the single deduped candidate with docker's provenance, got: %s", out)
	}
	if strings.Contains(out, "existing OMV-style stack") {
		t.Errorf("FAIL C12 (docker signal dedup): the lower-priority structural reason must not appear once docker already found this root: %s", out)
	}
	if strings.Contains(out, "ambiguous") || strings.Contains(out, "multiple possible") {
		t.Errorf("FAIL C12 (docker signal dedup): the same root found by two signals must collapse to ONE candidate, not read as ambiguous: %s", out)
	}
	if !strings.Contains(out, "stack directory: "+root+"/sentinel") {
		t.Errorf("FAIL C12 (docker signal dedup): expected %s/sentinel to be proposed, got: %s", root, out)
	}
	logPass(t, "PASS C12 (docker signal dedup: one candidate, docker's provenance wins over the structural scan's)")
}

// TestContainer_C12_ProvenanceShownInAmbiguousMenu: with two DISTINCT
// roots, one found only by docker, one found only by the structural
// scan, the ambiguous-choice menu must show both, numbered in signal
// priority order (docker first), each with the reason that produced it
// so the choice between them means something rather than being a quiz
// of bare paths.
func TestContainer_C12_ProvenanceShownInAmbiguousMenu(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-provenance-menu-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	const dockerRoot = "/opt/dockeronly"
	const structuralRoot = "/srv/dev-disk-by-uuid-provtest/docker-compose"
	exec_("mkdir -p " + dockerRoot + "/proj")
	exec_("mkdir -p " + structuralRoot + "/existingstack && " +
		"printf 'services: {}\\n' > " + structuralRoot + "/existingstack/existingstack.yml && " +
		"ln -sfn existingstack.yml " + structuralRoot + "/existingstack/compose.yml")
	lsBody := `[{"Name":"proj","Status":"running(1)","ConfigFiles":"` + dockerRoot + `/proj/proj.yml"}]`
	if out, errOut, code := exec_(dockerComposeLsStub(lsBody, 0)); code != 0 {
		t.Fatalf("FAIL C12 (provenance in ambiguous menu): could not install docker stub: %s %s", out, errOut)
	}

	out, errOut, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (provenance in ambiguous menu): --dry-run exit=%d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "  1) "+dockerRoot+" (1 compose project already here)") {
		t.Errorf("FAIL C12 (provenance in ambiguous menu): expected the docker-found root listed first with its reason, got: %s", out)
	}
	if !strings.Contains(out, "  2) "+structuralRoot+" (1 existing OMV-style stack found here)") {
		t.Errorf("FAIL C12 (provenance in ambiguous menu): expected the structurally-found root listed second with its reason, got: %s", out)
	}
	logPass(t, "PASS C12 (provenance in ambiguous menu: both candidates shown, docker first, each with why it was proposed)")
}

// TestContainer_C12_OmvConfdbadmFailsUgly: confirmed against the real
// OMV host, omv-confdbadm requires root and, run without it, does not
// print a clean error; it emits a multi-line Python traceback and
// exits non-zero. This shims the real binary's real location
// (/usr/sbin/omv-confdbadm, not /usr/local/bin, proves the absolute-path
// lookup, not just the command-v fallback the earlier config-fallback
// test already covers) with exactly that failure shape, in three
// variants, and asserts the config lookup degrades to "unknown" every
// time, never treats traceback content as a detected compose root, and
// never lets a non-JSON stdout past the shape guard even when the exit
// status alone would not have caught it.
func TestContainer_C12_OmvConfdbadmFailsUgly(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-confdbadm-ugly-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	exec_("mkdir -p /usr/sbin")

	traceback := `Traceback (most recent call last):
  File "/usr/sbin/omv-confdbadm", line 76, in <module>
    main()
  File "/usr/lib/python3/dist-packages/openmediavault/config/database.py", line 388, in _load
    raise SystemExit("Failed to load the configuration database")
SystemExit: Failed to load the configuration database`

	cases := []struct {
		name string
		stub string
	}{
		{
			name: "traceback on stderr, exit 1 (the real shape measured against the host)",
			stub: "#!/bin/sh\ncat >&2 <<'TB'\n" + traceback + "\nTB\nexit 1\n",
		},
		{
			name: "traceback misdirected to stdout, exit 1",
			stub: "#!/bin/sh\ncat <<'TB'\n" + traceback + "\nTB\nexit 1\n",
		},
		{
			name: "non-JSON stdout, exit 0 (a hypothetical future failure mode the exit-status check alone would not catch)",
			stub: "#!/bin/sh\necho 'warning: something unrelated happened'\nexit 0\n",
		},
		{
			// Reviewer's exact adversarial reproduction against the
			// pre-hardening code: a traceback-SHAPED blob (never starts
			// with '{' or '[') that nonetheless embeds a real-looking
			// "sharedfolderref" key and, further down, a well-formed
			// {"path": "/usr/lib/python3/dist-packages", "uuid": "..."}
			// object, exactly the kind of content a naive line-based
			// grep could scrape a Python library path out of and hand
			// back as a "detected" compose root, rc=0. The JSON-shape
			// guard (cfg must START with '{'/'[') is what stops this:
			// the overall stdout still starts with "Traceback ...", so
			// the whole answer is rejected before either grep pattern
			// ever runs, regardless of what text appears later in it.
			name: "traceback shape with embedded fake sharedfolderref/path/uuid JSON, exit 0",
			stub: "#!/bin/sh\ncat <<'TB'\n" +
				`Traceback (most recent call last):
  File "/usr/sbin/omv-confdbadm", line 76, in <module>
    main()
  "sharedfolderref": "11111111-1111-1111-1111-111111111111"
  File "/usr/lib/python3/dist-packages/openmediavault/config/database.py", line 388, in _load
    {"path": "/usr/lib/python3/dist-packages", "uuid": "11111111-1111-1111-1111-111111111111"}
SystemExit: Failed to load the configuration database` +
				"\nTB\nexit 0\n",
		},
	}

	for _, c := range cases {
		writeStub := "cat > /usr/sbin/omv-confdbadm <<'STUB'\n" + c.stub + "STUB\nchmod +x /usr/sbin/omv-confdbadm"
		if out, errOut, code := exec_(writeStub); code != 0 {
			t.Fatalf("FAIL C12 (omv-confdbadm fails ugly, %s): stub setup failed: %s %s", c.name, out, errOut)
		}

		// No structural candidate exists anywhere on this host, so the
		// only way "layout: omv" or a detected default could appear is
		// the config lookup mistaking the stub's failure output for a
		// real answer.
		out, _, code := exec_("/work/install.sh --dry-run 2>&1")
		if code != 0 {
			t.Fatalf("FAIL C12 (omv-confdbadm fails ugly, %s): --dry-run exit=%d: %s", c.name, code, out)
		}
		if !strings.Contains(out, "stack directory: /opt/sentinel") {
			t.Errorf("FAIL C12 (omv-confdbadm fails ugly, %s): expected the conventional /opt/sentinel fallback, got: %s", c.name, out)
		}
		if strings.Contains(out, "layout: omv") {
			t.Errorf("FAIL C12 (omv-confdbadm fails ugly, %s): a failing omv-confdbadm must never produce an omv layout: %s", c.name, out)
		}
		for _, mustNotAppear := range []string{"Traceback", "database.py", "dist-packages", "detected a possible"} {
			if strings.Contains(out, mustNotAppear) {
				t.Errorf("FAIL C12 (omv-confdbadm fails ugly, %s): traceback/failure content leaked into the decision (%q found): %s", c.name, mustNotAppear, out)
			}
		}
	}
	logPass(t, "PASS C12 (omv-confdbadm fails ugly: traceback on stdout or stderr, or non-JSON success output, all degrade to unknown)")
}

// TestContainer_C12_OmvConfdbadmSecondCallFailureChecked: the first
// omv-confdbadm call (conf.service.compose) can succeed cleanly while
// the second (conf.system.sharedfolder, resolving the uuid to a path)
// fails, a real, distinct failure mode (e.g. a database lock, a
// concurrent OMV UI edit) from the "not root at all" case the other
// test covers. Both calls' exit status must be checked independently;
// reviewer flagged this as unverified after 61e6eba.
func TestContainer_C12_OmvConfdbadmSecondCallFailureChecked(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-confdbadm-secondcall-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	exec_("mkdir -p /usr/sbin")

	stub := `#!/bin/sh
for a in "$@"; do id="$a"; done
if [ "$id" = "conf.service.compose" ]; then
  printf '{"sharedfolderref":"11111111-1111-1111-1111-111111111111"}\n'
  exit 0
elif [ "$id" = "conf.system.sharedfolder" ]; then
  echo "Traceback (most recent call last): second call failed" >&2
  exit 1
fi
`
	if out, errOut, code := exec_("cat > /usr/sbin/omv-confdbadm <<'STUB'\n" + stub + "STUB\nchmod +x /usr/sbin/omv-confdbadm"); code != 0 {
		t.Fatalf("FAIL C12 (omv-confdbadm second call failure): stub setup failed: %s %s", out, errOut)
	}

	// No structural candidate anywhere, and the config lookup's second
	// call always fails, the only correct outcome is the plain
	// /opt/sentinel fallback, never a guessed path.
	out, _, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (omv-confdbadm second call failure): --dry-run exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "stack directory: /opt/sentinel") {
		t.Errorf("FAIL C12 (omv-confdbadm second call failure): expected the conventional /opt/sentinel fallback when the second call fails, got: %s", out)
	}
	if strings.Contains(out, "layout: omv") {
		t.Errorf("FAIL C12 (omv-confdbadm second call failure): a failed second call must never produce an omv layout: %s", out)
	}
	logPass(t, "PASS C12 (omv-confdbadm second call failure: checked independently, degrades to unknown)")
}

// TestContainer_C12_OmvConfdbadmRealisticPrettyPrintedJSON: the earlier
// config-fallback test's stub emits {"uuid":...,"path":...} on ONE
// line, the only shape a line-adjacency `grep -B2` can match. Real
// `omv-confdbadm read` output was not confirmed to be formatted that
// way (root access to check was declined on the live host), so this
// drives the parser against a DELIBERATELY harder, still-plausible
// shape instead: pretty-printed, multi-line, with "path" appearing
// BEFORE "uuid" in the same object and several lines apart, the shape
// the flattened single-object match (order- and distance-independent)
// exists to handle, and the shape the original line-adjacency grep
// could not have matched at all.
func TestContainer_C12_OmvConfdbadmRealisticPrettyPrintedJSON(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-confdbadm-prettyprint-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	exec_("mkdir -p /usr/sbin")

	const realRoot = "/srv/dev-disk-by-uuid-realprettyprint/docker-compose"
	stub := `#!/bin/sh
for a in "$@"; do id="$a"; done
if [ "$id" = "conf.service.compose" ]; then
  cat <<'JSON'
{
    "enabled": true,
    "sharedfolderref": "11111111-1111-1111-1111-111111111111"
}
JSON
elif [ "$id" = "conf.system.sharedfolder" ]; then
  cat <<'JSON'
{
        "comment": "",
        "mntentref": "22222222-2222-2222-2222-222222222222",
        "name": "docker-compose",
        "privileges": {
            "privilege": []
        },
        "reldirpath": "docker-compose/",
    "uuid": "11111111-1111-1111-1111-111111111111"
}
JSON
elif [ "$id" = "conf.system.filesystem.mountpoint" ]; then
  cat <<'JSON'
{
    "dir": "/srv/dev-disk-by-uuid-realprettyprint",
    "fsname": "/dev/disk/by-uuid/realprettyprint",
    "type": "ext4",
    "uuid": "22222222-2222-2222-2222-222222222222"
}
JSON
fi
`
	if out, errOut, code := exec_("mkdir -p " + realRoot + " && cat > /usr/sbin/omv-confdbadm <<'STUB'\n" + stub + "STUB\nchmod +x /usr/sbin/omv-confdbadm"); code != 0 {
		t.Fatalf("FAIL C12 (omv-confdbadm pretty-printed): stub setup failed: %s %s", out, errOut)
	}

	// realRoot has no siblings (structural detection is inconclusive),
	// so only a correctly parsed config answer can produce omv here. The
	// shape is OMV's real one: a shared folder has no "path", it carries
	// "mntentref" and "reldirpath" and the absolute path is the referenced
	// mountpoint's "dir" with the relative part joined on. It also carries a
	// nested "privileges" value, so the record is not a flat brace-delimited
	// object, which is what a non-greedy "{[^{}]*}" match would settle on.
	out, _, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (omv-confdbadm pretty-printed): --dry-run exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "stack directory: "+realRoot+"/sentinel") || !strings.Contains(out, "layout: omv") {
		t.Errorf("FAIL C12 (omv-confdbadm pretty-printed): pretty-printed, alphabetically-ordered JSON with a nested privileges value was not resolved to a path: %s", out)
	}
	logPass(t, "PASS C12 (omv-confdbadm pretty-printed: order- and distance-independent parsing confirmed against a harder, still-plausible shape)")
}

// TestContainer_C12_OmvConfdbadmLeadingWhitespace: `$()` strips only
// TRAILING newlines from command substitution output, never leading
// whitespace, a well-formed answer that happens to begin with a blank
// line or leading spaces would otherwise be rejected by the JSON-shape
// guard (`case "$cfg" in '{'*)`) exactly like a real failure would be.
// The real omv-confdbadm output shape is still unverified, so this
// stub deliberately leads BOTH calls' output with a blank line and
// leading spaces before the opening brace, plausible, not exotic,
// and confirms the trim added in front of the shape check tolerates it
// rather than leaving the fresh-install zero-stacks case permanently
// broken by a formatting detail this script never actually depends on.
func TestContainer_C12_OmvConfdbadmLeadingWhitespace(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-confdbadm-leadingws-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	exec_("mkdir -p /usr/sbin")

	const emptyRoot = "/srv/dev-disk-by-uuid-leadingws/docker-compose"
	stub := `#!/bin/sh
for a in "$@"; do id="$a"; done
if [ "$id" = "conf.service.compose" ]; then
  printf '\n  {"sharedfolderref":"11111111-1111-1111-1111-111111111111"}\n'
elif [ "$id" = "conf.system.sharedfolder" ]; then
  printf '\n  {"uuid":"11111111-1111-1111-1111-111111111111","mntentref":"22222222-2222-2222-2222-222222222222","reldirpath":"docker-compose/","privileges":{"privilege":[]}}\n'
elif [ "$id" = "conf.system.filesystem.mountpoint" ]; then
  printf '\n  {"uuid":"22222222-2222-2222-2222-222222222222","dir":"/srv/dev-disk-by-uuid-leadingws","type":"ext4"}\n'
fi
`
	if out, errOut, code := exec_("mkdir -p " + emptyRoot + " && cat > /usr/sbin/omv-confdbadm <<'STUB'\n" + stub + "STUB\nchmod +x /usr/sbin/omv-confdbadm"); code != 0 {
		t.Fatalf("FAIL C12 (omv-confdbadm leading whitespace): stub setup failed: %s %s", out, errOut)
	}

	// emptyRoot has no siblings, so only a correctly-trimmed, correctly
	// parsed config answer can produce omv here.
	out, _, code := exec_("/work/install.sh --dry-run 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (omv-confdbadm leading whitespace): --dry-run exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "stack directory: "+emptyRoot+"/sentinel") || !strings.Contains(out, "layout: omv") {
		t.Errorf("FAIL C12 (omv-confdbadm leading whitespace): a leading blank line/spaces before well-formed JSON must not be treated as a shape-guard failure: %s", out)
	}
	logPass(t, "PASS C12 (omv-confdbadm leading whitespace: benign leading blank line/spaces tolerated, not mistaken for a malformed answer)")
}

// TestContainer_C12_DryRunVerbAudit: every "note" line that describes an
// action this script would take must say "would" under --dry-run, never
// claim the action already happened. A previous round fixed exactly the
// lines a reviewer had quoted (ensure_dir's shared helper, step1, step2)
// and left the same defect standing in step3/4/5's "updated"/"collapsed"
// wording, which is reachable via render_managed_block returning
// "changed" identically for --dry-run and a real write.
func TestContainer_C12_DryRunVerbAudit(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-verbaudit-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// Run 1: a fresh container (nothing installed, no rasdaemon service,
	// no stack directory yet) exercises ensure_dir, step1 and step2
	// together, none of these three depend on mail being configured.
	out1, errOut1, code1 := exec_("/work/install.sh --dry-run --stack-dir /opt/newstack 2>&1")
	if code1 != 0 {
		t.Fatalf("FAIL C12 (dry-run verb audit, run 1): exit=%d: %s %s", code1, out1, errOut1)
	}
	for _, mustNotAppear := range []string{
		"stack directory: creating ",
		"mailrise directory: creating ",
		"step1 packages: installing ",
		"step2 rasdaemon: enabling and starting",
	} {
		if strings.Contains(out1, mustNotAppear) {
			t.Errorf("FAIL C12 (dry-run verb audit, run 1): claims completed action %q during --dry-run: %s", mustNotAppear, out1)
		}
	}
	for _, mustAppear := range []string{
		"stack directory: would create ",
		"mailrise directory: would create ",
		"step1 packages: would install ",
		"step2 rasdaemon: would enable and start",
	} {
		if !strings.Contains(out1, mustAppear) {
			t.Errorf("FAIL C12 (dry-run verb audit, run 1): missing conditional phrasing %q: %s", mustAppear, out1)
		}
	}

	// Run 2: mail credentials present via --env-file, /etc/zfs/zed.d
	// present, but /etc/msmtprc, /etc/smartd.conf and zed.rc all still
	// absent, exercises step3/4/5's "updated" branch (the file does not
	// exist yet, so render_managed_block reports "changed" even though
	// --dry-run writes nothing).
	exec_("mkdir -p /etc/zfs/zed.d && printf 'MAILRISE_SMTP_USER=u\\nMAILRISE_SMTP_PASS=p\\n' > /root/creds.env")
	out2, errOut2, code2 := exec_("/work/install.sh --dry-run --env-file /root/creds.env 2>&1")
	if code2 != 0 {
		t.Fatalf("FAIL C12 (dry-run verb audit, run 2): exit=%d: %s %s", code2, out2, errOut2)
	}
	for _, mustNotAppear := range []string{
		"step3 /etc/msmtprc: updated",
		"step4 /etc/smartd.conf: updated",
		"step5 /etc/zfs/zed.d/zed.rc: updated",
	} {
		if strings.Contains(out2, mustNotAppear) {
			t.Errorf("FAIL C12 (dry-run verb audit, run 2): claims completed action %q during --dry-run: %s", mustNotAppear, out2)
		}
	}
	for _, mustAppear := range []string{
		"step3 /etc/msmtprc: would be updated",
		// step4/step5 are the opt-in monitoring reconfiguration, declined
		// by default, --dry-run/--check preview what a "y" answer would
		// write but must never phrase it as pending work: a real
		// unattended run would not make this change unless asked (see
		// contracts/runtime.md R5's "opt-in change that defaults to No
		// is not drift"). TestContainer_C12_CheckExitCodeMatchesChanged
		// (below) is the assertion that this distinction also holds for
		// `changed`/`--check`'s exit code, not just the wording here.
		"step4 /etc/smartd.conf: available, would be updated if enabled (opt-in, defaults to No, not counted as drift)",
		"step5 /etc/zfs/zed.d/zed.rc: available, would be updated if enabled (opt-in, defaults to No, not counted as drift)",
	} {
		if !strings.Contains(out2, mustAppear) {
			t.Errorf("FAIL C12 (dry-run verb audit, run 2): missing conditional phrasing %q: %s", mustAppear, out2)
		}
	}

	// Run 3: a pre-existing /etc/msmtprc with TWO managed blocks (never
	// produced by this script itself, but reachable from a restored
	// backup or an interrupted run) exercises the "collapsed" wording
	// specifically.
	exec_(`cat > /etc/msmtprc <<'EOF'
# >>> agentic-server-supervisor (managed) >>>
stale one
# <<< agentic-server-supervisor (managed) <<<
# >>> agentic-server-supervisor (managed) >>>
stale two
# <<< agentic-server-supervisor (managed) <<<
EOF`)
	out3, errOut3, code3 := exec_("/work/install.sh --dry-run --env-file /root/creds.env 2>&1")
	if code3 != 0 {
		t.Fatalf("FAIL C12 (dry-run verb audit, run 3): exit=%d: %s %s", code3, out3, errOut3)
	}
	if strings.Contains(out3, "step3 /etc/msmtprc: collapsed") {
		t.Errorf("FAIL C12 (dry-run verb audit, run 3): claims a completed collapse during --dry-run: %s", out3)
	}
	if !strings.Contains(out3, "step3 /etc/msmtprc: would collapse 2 managed blocks into 1") {
		t.Errorf("FAIL C12 (dry-run verb audit, run 3): missing conditional collapse phrasing: %s", out3)
	}
	logPass(t, "PASS C12 (dry-run verb audit: every action note says would, not did)")
}

// TestContainer_C12_StackDryRunSummaryHonest: the run summary printed at
// the end is the last thing an operator reads before deciding to write
// to a real host. The per-action lines already say "[dry-run] would
// ..."; the summary lines that roll those up must say the same, not
// reuse the same wording a real run uses for something it actually did.
func TestContainer_C12_StackDryRunSummaryHonest(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-dryrun-summary-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	out, _, code := exec_("/work/install.sh --dry-run --stack-dir /opt/sentinel 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (stack dry-run summary): exit=%d: %s", code, out)
	}
	for _, wantWould := range []string{
		"would be written/updated",
		"would write",
	} {
		if !strings.Contains(out, wantWould) {
			t.Errorf("FAIL C12 (stack dry-run summary): missing conditional phrasing %q, a --dry-run summary must not claim work happened: %s", wantWould, out)
		}
	}
	for _, mustNotClaim := range []string{
		"stack compose file: /opt/sentinel/docker-compose.yml written/updated",
		"stack env: wrote ",
		"step6 JOURNAL_GID: setting to",
	} {
		if strings.Contains(out, mustNotClaim) {
			t.Errorf("FAIL C12 (stack dry-run summary): claims completed work that --dry-run never did (%q): %s", mustNotClaim, out)
		}
	}
	if out, _, _ := exec_("test -e /opt/sentinel && echo EXISTS || echo ABSENT"); !strings.Contains(out, "ABSENT") {
		t.Errorf("FAIL C12 (stack dry-run summary): --dry-run created /opt/sentinel despite the summary being about phrasing, not writes")
	}
	logPass(t, "PASS C12 (stack dry-run summary: says would, not did)")
}

// TestContainer_C12_StackEnvFileBackCompat: --env-file must behave
// exactly as it did before this whole stack-creation path existed, no
// stack directory resolved, no compose file fetched, no prompting.
// Every prior caller of this script keeps working unmodified.
func TestContainer_C12_StackEnvFileBackCompat(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-backcompat-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	exec_("mkdir -p /tmp/oldstyle && printf 'MAILRISE_SMTP_USER=u\\nMAILRISE_SMTP_PASS=p\\n' > /tmp/oldstyle/.env")

	out1, errOut1, code1 := exec_("/work/install.sh --env-file /tmp/oldstyle/.env 2>&1")
	if code1 != 0 && code1 != 75 {
		t.Fatalf("FAIL C12 (stack env-file back-compat): exit=%d (want 0 or 75, transient in this rootless test env): %s %s", code1, out1, errOut1)
	}
	if strings.Contains(out1, "stack directory:") {
		t.Errorf("FAIL C12 (stack env-file back-compat): --env-file must not trigger stack resolution: %s", out1)
	}
	if out, _, _ := exec_("test -e /opt/sentinel && echo EXISTS || echo ABSENT"); !strings.Contains(out, "ABSENT") {
		t.Errorf("FAIL C12 (stack env-file back-compat): /opt/sentinel was created despite --env-file being given explicitly")
	}
	logPass(t, "PASS C12 (stack env-file back-compat: unchanged behavior)")
}

// TestContainer_C12_StackMutualExclusion: --env-file and --stack-dir
// name two different, incompatible things this script could do, and
// silently picking one over the other would surprise whichever caller
// lost.
func TestContainer_C12_StackMutualExclusion(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12 (stack mutual exclusion): cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}
	out, errOut, code = runCmd(t, 30*time.Second, dockerBin(), "run", "--rm",
		"-v", root+":/work:ro",
		"debian:trixie-slim", "/work/install.sh", "--env-file", "/tmp/x", "--stack-dir", "/tmp/y")
	if code != 64 {
		t.Fatalf("FAIL C12 (stack mutual exclusion): exit=%d, want 64: %s %s", code, out, errOut)
	}
	logPass(t, "PASS C12 (stack mutual exclusion: --env-file + --stack-dir rejected)")
}

// TestContainer_C12_MailriseConfHostileSecrets: BLOCKER, mailrise.conf
// was rendered with `sed -e "s/REPLACE_X/${value}/g"`, and sed's
// replacement text is not literal: `/` collides with the s///
// delimiter (the expression itself breaks), and `&` means "the whole
// match" (spliced into the substitution, silently corrupting the
// credential). Measured against a real run before the fix: a password
// containing `/` crashed sed and left a ZERO-BYTE mailrise.conf that
// the summary still reported as "written"; a password containing `&`
// produced no error at all (exit 0) while mailrise.conf held
// "sentinel: SecretREPLACE_SMTP_PASSPass" instead of the real password
// , the .env and mailrise.conf copies of the SAME credential silently
// disagreeing, which is exactly what makes mailrise reject AUTH from
// the supervisor, smartd and ZED while `sentinel health` stays green.
//
// The fix takes sed out of this substitution entirely (bash
// `${var//pat/rep}`, whose replacement text has no delimiter or `&`
// semantics) and adds a fail-closed post-condition (no leftover
// REPLACE_ token survives to be written). This test is deliberately
// NOT an escaping test, escaping is a blocklist someone always finds
// a gap in, it exercises the actual invariant: the credential in the
// stack's env file and the credential rendered into mailrise.conf must
// be byte-identical, for values containing every character the
// original bug depended on plus the ones most likely to reveal a
// sibling bug in the same substitution mechanism.
func TestContainer_C12_MailriseConfHostileSecrets(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-hostile-secrets-test"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	longValue := strings.Repeat("x-long-secret-", 15) // 210 chars
	cases := []struct {
		name string
		pass string
	}{
		{"slash", "p/a/s/s"},
		{"ampersand", "Secret&Pass"},
		{"slash and ampersand together (the exact reported repro)", "p/a$s&word"},
		{"backslash", `back\slash\pass`},
		{"dollar", "dollar$sign$pass"},
		{"backtick", "back`tick`pass"},
		{"double quote", `double"quote"pass`},
		{"single quote", "single'quote'pass"},
		{"spaces", "pass with spaces in it"},
		{"leading dash", "-leading-dash-pass"},
		{"long value", longValue},
		{"kitchen sink", "p/a$s&w`d\"q'uo te-x" + longValue},
	}

	for i, c := range cases {
		stackDir := fmt.Sprintf("/opt/hostile%d", i)
		token := fmt.Sprintf("12345678%d:TOKEN%d", i, i)
		chat := fmt.Sprintf("-100%d", i)
		envSetup := fmt.Sprintf(`mkdir -p %s && cat > %s/.env <<'ENVEOF'
TELEGRAM_BOT_TOKEN=%s
TELEGRAM_CHAT_ID=%s
MAILRISE_SMTP_USER=sentinel
MAILRISE_SMTP_PASS=%s
ENVEOF`, stackDir, stackDir, token, chat, c.pass)
		if out, errOut, code := exec_(envSetup); code != 0 {
			t.Fatalf("FAIL C12 (hostile secrets, %s): env setup failed: %s %s", c.name, out, errOut)
		}

		out, errOut, code := exec_("/work/install.sh --stack-dir " + stackDir + " 2>&1")
		if code != 0 && code != 75 {
			t.Fatalf("FAIL C12 (hostile secrets, %s): exit=%d (want 0 or 75): %s %s", c.name, code, out, errOut)
		}

		mailriseConf, _, _ := exec_("cat " + stackDir + "/mailrise/mailrise.conf 2>&1")
		if mailriseConf == "" {
			t.Errorf("FAIL C12 (hostile secrets, %s): mailrise.conf is empty: %s", c.name, out)
			continue
		}
		if strings.Contains(mailriseConf, "REPLACE_") {
			t.Errorf("FAIL C12 (hostile secrets, %s): mailrise.conf still contains an unreplaced REPLACE_ token: %s", c.name, mailriseConf)
		}
		// The actual invariant: byte-identical between the two places
		// this credential now lives, not merely "some substitution
		// happened". This is what neither of the reviewer's two
		// reproduced runs had.
		if !strings.Contains(mailriseConf, c.pass) {
			t.Errorf("FAIL C12 (hostile secrets, %s): mailrise.conf does not contain the password byte-for-byte %q: %s", c.name, c.pass, mailriseConf)
		}
		envContent, _, _ := exec_("cat " + stackDir + "/.env 2>&1")
		if !strings.Contains(envContent, "MAILRISE_SMTP_PASS="+c.pass) {
			t.Errorf("FAIL C12 (hostile secrets, %s): .env does not carry the password unchanged: %s", c.name, envContent)
		}
	}
	logPass(t, "PASS C12 (mailrise.conf hostile secrets: byte-identical credential in .env and mailrise.conf for every hostile value, never a silent-success corruption)")
}

// TestContainer_C12_MsmtprcHostileSecrets: BLOCKER (reviewer round 2),
// render_managed_block wrote step3's managed block via `awk -v
// repl="$desired_block" '...'`, and `awk -v` performs ESCAPE-SEQUENCE
// PROCESSING on the assigned value: `awk -v r='p\tb' 'BEGIN{print r}'`
// prints a literal TAB, not the four characters p\tb. desired_block for
// step3 embeds `password ${smtp_pass}`, so a password containing a
// literal backslash sequence reached /etc/msmtprc mangled while .env
// kept the real bytes. Same consequence as the mailrise.conf blocker,
// one file over: msmtp authenticates with the wrong password, mailrise
// rejects AUTH, every smartd/ZED alert is dropped silently, and
// `sentinel health` stays green.
//
// TestContainer_C12_MailriseConfHostileSecrets does NOT catch this,
// its container has no msmtp installed, so MAIL_OK stays 0 and step3
// never runs at all. This test installs msmtp/msmtp-mta specifically so
// step3 genuinely executes (verified below by requiring "step3
// /etc/msmtprc: updated" to actually appear in the first run's output,
// not merely assumed), then asserts (i) /etc/msmtprc's password line is
// byte-identical to the .env value, for tab/backslash/double-quote,
// the family this whole round of fixes was about, and (ii) idempotency
// has teeth: with the bug present, `existing` (mangled) never equals
// `desired_block` (raw), so every second run re-"converges", restarts
// msmtp-dependent services, and reports changed>0 forever even though
// the file's sha256 stops changing, the R5 idempotency contract's
// "second run reports changed=0" half is exactly what that masks.
func TestContainer_C12_MsmtprcHostileSecrets(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12 (msmtprc hostile secrets): cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	cases := []struct {
		name string
		pass string
	}{
		{"tab", `pass\tword`},
		{"backslash", `pass\\word`},
		{"double quote", `pass\"word`},
	}

	for i, c := range cases {
		name := fmt.Sprintf("sentinel-c12-msmtprc-hostile-%d", i)
		runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
		if _, errOut, code := runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
			"-v", root+":/work:ro", "debian:trixie-slim", "sleep", "300"); code != 0 {
			skipUnlessCI(t, "C12 (msmtprc hostile secrets, %s): could not start throwaway container: %s", c.name, errOut)
		}
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

		prep := `set -e
cp -r /work /root/repo
apt-get update -qq
apt-get install -y -qq systemd msmtp msmtp-mta >/dev/null 2>&1
chmod +x /root/repo/install.sh`
		if out, errOut, code := exec_(prep); code != 0 {
			skipUnlessCI(t, "C12 (msmtprc hostile secrets, %s): prep failed (msmtp package unavailable?): %s %s", c.name, out, errOut)
		}
		// A quoted heredoc, not printf with the password embedded in the
		// FORMAT STRING: printf itself interprets \\t/\\\\ as escapes in
		// its own format argument, which would corrupt the very bytes this
		// test exists to verify survive intact -- before install.sh ever
		// saw them. A quoted heredoc delimiter performs no expansion at
		// all, so c.pass reaches the file exactly as written in Go.
		envSetup := "cat > /root/repo/.env <<'ENVEOF'\nMAILRISE_SMTP_USER=sentinel\nMAILRISE_SMTP_PASS=" + c.pass + "\nENVEOF"
		if out, errOut, code := exec_(envSetup); code != 0 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, %s): env setup failed: %s %s", c.name, out, errOut)
		}

		out1, errOut1, code1 := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
		if code1 != 0 && code1 != 75 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, %s): first run exit=%d (want 0 or 75): %s %s", c.name, code1, out1, errOut1)
		}
		// Proves step3 genuinely ran (not skipped for lack of
		// msmtp/credentials) rather than assuming MAIL_OK ended up 1,
		// the exact gap that let this bug through the first hostile
		// secrets test undetected.
		if !strings.Contains(out1, "step3 /etc/msmtprc: updated") {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, %s): step3 did not run on the first pass (msmtp not actually installed in this environment?): %s %s", c.name, out1, errOut1)
		}

		msmtprc, _, _ := exec_("grep '^password ' /etc/msmtprc")
		wantLine := "password " + c.pass
		if strings.TrimSpace(msmtprc) != wantLine {
			t.Errorf("FAIL C12 (msmtprc hostile secrets, %s): /etc/msmtprc password line = %q, want %q (byte-identical to .env)", c.name, strings.TrimSpace(msmtprc), wantLine)
		}

		out2, errOut2, code2 := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
		if code2 != 0 && code2 != 75 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, %s): second run exit=%d (want 0 or 75): %s %s", c.name, code2, out2, errOut2)
		}
		if !strings.Contains(out2, "step3 /etc/msmtprc: already converged") {
			t.Errorf("FAIL C12 (msmtprc hostile secrets, %s): second run did not converge for step3 (idempotency broken by a mangled password that never matches its own re-render): %s", c.name, out2)
		}
	}
	logPass(t, "PASS C12 (msmtprc hostile secrets: /etc/msmtprc byte-identical to .env for tab/backslash/double-quote, second run converges)")
}

// TestContainer_C12_MsmtprcHostileSecretsBlockRewrite: BLOCKER
// (reviewer, round 3), render_managed_block has TWO write paths. A
// brand-new /etc/msmtprc goes through `printf '\n%s\n' "$desired_block"`
// (plain, correct even with the buggy `awk -v`). Only an EXISTING
// managed block that DIFFERS from the desired one goes through the awk
// rewrite, the path the `env`/`ENVIRON` fix in render_managed_block
// actually protects. TestContainer_C12_MsmtprcHostileSecrets writes
// `.env` once and installs TWICE with the SAME password: run 1 takes
// the printf path (correct either way); run 2 finds the block already
// matching and reports "already converged" WITHOUT ever invoking awk.
// Reverting the fix back to `awk -v` and running that test measured
// green, the mutant passed, because the line the fix touches was never
// executed. Kept as a separate test (not folded into the existing one)
// so the two code paths fail independently and a future regression
// names which one broke.
//
// This test forces the awk path deliberately: prime with an ORDINARY
// password so the managed block is created via the printf path, THEN
// switch to the hostile password and install again, the block now
// exists AND differs, which is the only way to reach the rewrite.
func TestContainer_C12_MsmtprcHostileSecretsBlockRewrite(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12 (msmtprc hostile secrets, block rewrite): cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	cases := []struct {
		name string
		pass string
	}{
		{"tab", `pass\tword`},
		{"backslash", `pass\\word`},
		{"double quote", `pass\"word`},
	}

	for i, c := range cases {
		name := fmt.Sprintf("sentinel-c12-msmtprc-rewrite-%d", i)
		runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
		if _, errOut, code := runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
			"-v", root+":/work:ro", "debian:trixie-slim", "sleep", "300"); code != 0 {
			skipUnlessCI(t, "C12 (msmtprc hostile secrets, block rewrite, %s): could not start throwaway container: %s", c.name, errOut)
		}
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

		prep := `set -e
cp -r /work /root/repo
apt-get update -qq
apt-get install -y -qq systemd msmtp msmtp-mta >/dev/null 2>&1
chmod +x /root/repo/install.sh`
		if out, errOut, code := exec_(prep); code != 0 {
			skipUnlessCI(t, "C12 (msmtprc hostile secrets, block rewrite, %s): prep failed (msmtp package unavailable?): %s %s", c.name, out, errOut)
		}

		// Priming run: an ORDINARY password, so the managed block gets
		// created via the printf path (correct regardless of the bug).
		primeEnv := "cat > /root/repo/.env <<'ENVEOF'\nMAILRISE_SMTP_USER=sentinel\nMAILRISE_SMTP_PASS=firstpass\nENVEOF"
		if out, errOut, code := exec_(primeEnv); code != 0 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): priming env setup failed: %s %s", c.name, out, errOut)
		}
		outPrime, errOutPrime, codePrime := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
		if codePrime != 0 && codePrime != 75 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): priming run exit=%d (want 0 or 75): %s %s", c.name, codePrime, outPrime, errOutPrime)
		}
		if !strings.Contains(outPrime, "step3 /etc/msmtprc: updated") {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): priming run did not write the managed block (msmtp not actually installed in this environment?): %s %s", c.name, outPrime, errOutPrime)
		}

		// Switch to the hostile password. A quoted heredoc, not printf
		// with the password embedded in the FORMAT STRING: printf itself
		// interprets \t/\\ as escapes in its own format argument, which
		// would corrupt the very bytes this test exists to verify
		// survive intact -- before install.sh ever saw them.
		envSetup := "cat > /root/repo/.env <<'ENVEOF'\nMAILRISE_SMTP_USER=sentinel\nMAILRISE_SMTP_PASS=" + c.pass + "\nENVEOF"
		if out, errOut, code := exec_(envSetup); code != 0 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): env setup failed: %s %s", c.name, out, errOut)
		}

		out1, errOut1, code1 := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
		if code1 != 0 && code1 != 75 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): rewrite run exit=%d (want 0 or 75): %s %s", c.name, code1, out1, errOut1)
		}
		// The block exists (from priming) AND differs (new password),
		// "updated" here can only mean the awk rewrite path actually ran,
		// not a converged no-op.
		if !strings.Contains(out1, "step3 /etc/msmtprc: updated") {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): step3 did not rewrite the differing block: %s %s", c.name, out1, errOut1)
		}

		msmtprc, _, _ := exec_("grep '^password ' /etc/msmtprc")
		wantLine := "password " + c.pass
		if strings.TrimSpace(msmtprc) != wantLine {
			t.Errorf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): /etc/msmtprc password line = %q, want %q (byte-identical to .env, via the awk rewrite path)", c.name, strings.TrimSpace(msmtprc), wantLine)
		}

		out2, errOut2, code2 := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
		if code2 != 0 && code2 != 75 {
			t.Fatalf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): second run exit=%d (want 0 or 75): %s %s", c.name, code2, out2, errOut2)
		}
		if !strings.Contains(out2, "step3 /etc/msmtprc: already converged") {
			t.Errorf("FAIL C12 (msmtprc hostile secrets, block rewrite, %s): a second run after the awk rewrite did not converge (the rewritten content does not match its own re-render): %s", c.name, out2)
		}
	}
	logPass(t, "PASS C12 (msmtprc hostile secrets, block rewrite: the awk path specifically writes /etc/msmtprc byte-identical to .env and then converges)")
}

// TestContainer_C12_StackInteractiveSecrets: with a real controlling
// terminal, the three prompts are answered and the values land exactly
// where they belong, sentinel.env, and mailrise.conf's two targets
// (sentinel and omv both address the same Telegram chat). `script`
// allocates the pty; install.sh's own content still goes to the
// child's stdin exactly as `curl | bash` would, decoupled from the
// terminal `read -rs`/`read -r` reads from at /dev/tty.
func TestContainer_C12_StackInteractiveSecrets(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-interactive-test"
	// This test's subject is step0b_secrets' prompting, not package
	// installation, starting from the package-bearing base keeps
	// step1's real apt-get off the pty's timing, the same reasoning
	// the monitoring-prompt tests already use. Reaching step4's own
	// prompt (answered below) no longer has step1's real install
	// stacked in front of it.
	startC12ContainerFrom(t, name, c12BaseWithPackages(t))
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	if out, _, code := exec_("command -v script"); code != 0 {
		skipUnlessCI(t, "C12 (stack interactive): the `script` utility is not available in this environment: %s", out)
	}
	// A structurally OMV-shaped root (one sibling stack, the sentinel.yml
	// + compose.yml symlink shape), detection is structural now, not a
	// bare "/docker-compose exists" check, so this must actually look
	// like an OMV compose root for --stack-dir under it to get the OMV
	// layout (sentinel.env) this test asserts against.
	exec_("mkdir -p /docker-compose/existingstack && " +
		"printf 'services: {}\\n' > /docker-compose/existingstack/existingstack.yml && " +
		"ln -sfn existingstack.yml /docker-compose/existingstack/compose.yml")

	const token = "123456789:TESTTOKEN_ABCDEF"
	const chat = "-100999888"
	const smtpPass = "test-smtp-secret-value"
	// A real mailrise password answered here means MAIL_OK genuinely
	// becomes 1 once step1 installs msmtp for real, the same
	// situation a real operator typing these three answers would be
	// in, so step4 asks a fourth question. A trailing blank Enter
	// declines it (the documented default), which is what an operator
	// here to configure notifications, not smartd, would type; this
	// test's own subject (the three secrets landing correctly) is
	// unaffected either way.
	answers := token + "\n" + chat + "\n" + smtpPass + "\n\n"
	driveCmd := fmt.Sprintf(
		`printf %s | script -qec "bash -s -- --stack-dir /docker-compose/sentinel < /work/install.sh" /tmp/script.log`,
		shellQuote(answers),
	)
	outI, errOutI, codeI := exec_(driveCmd)
	if codeI != 0 && codeI != 75 {
		t.Fatalf("FAIL C12 (stack interactive): exit=%d (want 0 or 75, transient in this rootless test env): %s %s", codeI, outI, errOutI)
	}

	envContent, _, _ := exec_("cat /docker-compose/sentinel/sentinel.env")
	for _, want := range []string{
		"TELEGRAM_BOT_TOKEN=" + token,
		"TELEGRAM_CHAT_ID=" + chat,
		"MAILRISE_SMTP_PASS=" + smtpPass,
		"MAILRISE_SMTP_USER=sentinel",
	} {
		if !strings.Contains(envContent, want) {
			t.Errorf("FAIL C12 (stack interactive): sentinel.env missing %q: %s", want, envContent)
		}
	}

	mailriseContent, _, _ := exec_("cat /docker-compose/sentinel/mailrise/mailrise.conf")
	wantURL := "tgram://" + token + "/" + chat
	if strings.Count(mailriseContent, wantURL) != 2 {
		t.Errorf("FAIL C12 (stack interactive): mailrise.conf must carry %q exactly twice (sentinel: and omv: targets), got: %s", wantURL, mailriseContent)
	}
	if !strings.Contains(mailriseContent, "sentinel: "+smtpPass) {
		t.Errorf("FAIL C12 (stack interactive): mailrise.conf smtp auth missing the password: %s", mailriseContent)
	}
	if mode, _, _ := exec_("stat -c '%a' /docker-compose/sentinel/mailrise/mailrise.conf"); strings.TrimSpace(mode) != "644" {
		t.Errorf("FAIL C12 (stack interactive): mailrise.conf mode = %s, want 644 (0600 crash-loops the container)", strings.TrimSpace(mode))
	}
	if mode, _, _ := exec_("stat -c '%a' /docker-compose/sentinel/sentinel.env"); strings.TrimSpace(mode) != "600" {
		t.Errorf("FAIL C12 (stack interactive): sentinel.env mode = %s, want 600", strings.TrimSpace(mode))
	}
	logPass(t, "PASS C12 (stack interactive: prompted values land in sentinel.env and both mailrise.conf targets)")
}

// TestContainer_C12_StackInteractiveEmptyTokenFailsClosed: a real
// terminal answering Enter at the Telegram prompts is not the same as
// having no terminal at all, but it is the identical "I still don't
// have this" outcome by a different route, and MAILRISE_SMTP_USER/
// PASS only escape this exact gap by luck (compute_mail_status re-reads
// the env file independently and catches a blank password on its own);
// TELEGRAM_BOT_TOKEN/CHAT_ID have no such second check, and compose
// does not :?-guard them. An install.sh that let this through
// would report success while producing a stack that never delivers a
// notification and, once mailrise.conf's bind-mount target is missing,
// crash-loops mailrise outright.
func TestContainer_C12_StackInteractiveEmptyTokenFailsClosed(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-emptytoken-test"
	// Same reasoning as TestContainer_C12_StackInteractiveSecrets: this
	// test's subject is the empty-answer fail-closed behavior, not
	// package installation.
	startC12ContainerFrom(t, name, c12BaseWithPackages(t))
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	if out, _, code := exec_("command -v script"); code != 0 {
		skipUnlessCI(t, "C12 (stack empty token): the `script` utility is not available in this environment: %s", out)
	}
	exec_("mkdir -p /docker-compose/existingstack && " +
		"printf 'services: {}\\n' > /docker-compose/existingstack/existingstack.yml && " +
		"ln -sfn existingstack.yml /docker-compose/existingstack/compose.yml")

	// Enter, Enter (empty token, empty chat id), then a real mailrise
	// password, the shape the reviewer measured against a real pty.
	// The empty token is this test's whole point (MISSING_ENV_INPUT,
	// exit 78 checked at the very end of the run, after every step),
	// but the real password still makes MAIL_OK become 1 independently
	// of the token being empty, step4 asks a fourth question before
	// that final exit-78 check is ever reached, so a trailing blank
	// Enter (the documented default) answers it without touching what
	// this test actually verifies.
	const smtpPass = "test-smtp-secret-value"
	answers := "\n\n" + smtpPass + "\n\n"
	driveCmd := fmt.Sprintf(
		`printf %s | script -qec "bash -s -- --stack-dir /docker-compose/sentinel < /work/install.sh" /tmp/script2.log`,
		shellQuote(answers),
	)
	out, errOut, code := exec_(driveCmd)
	if code != 78 {
		t.Fatalf("FAIL C12 (stack empty token): exit=%d, want 78 (an empty answer at a real prompt must be treated as missing input, not accepted), stdout=%q stderr=%q", code, out, errOut)
	}
	if envContent, _, _ := exec_("cat /docker-compose/sentinel/sentinel.env 2>&1"); strings.Contains(envContent, "TELEGRAM_BOT_TOKEN=") {
		t.Errorf("FAIL C12 (stack empty token): TELEGRAM_BOT_TOKEN written despite an empty answer: %s", envContent)
	}
	if out, _, _ := exec_("test -e /docker-compose/sentinel/mailrise/mailrise.conf && echo EXISTS || echo ABSENT"); !strings.Contains(out, "ABSENT") {
		t.Errorf("FAIL C12 (stack empty token): mailrise.conf was written despite the token/chat id being empty")
	}
	logPass(t, "PASS C12 (stack empty token: Enter at a real prompt fails closed, not silently accepted)")
}

// shellQuote wraps s in single quotes for embedding in a shell command
// string, escaping any single quote in s itself. Only used to build the
// `printf %s | script ...` driver above with fixed, test-controlled
// values, not a general-purpose escaper.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestContainer_C12_CollapsesDuplicateManagedBlocks: everything between
// our own markers is OUR content, never the operator's (unlike the
// pre-existing smartd -m line, which survives as a comment specifically
// because it belongs to them), so a file that somehow ends up with TWO
// managed blocks (a half-finished run, a restored backup, a merge) is
// our own mess to clean up, not a state a human has to resolve by hand.
// Pre-seeds /etc/smartd.conf with two managed blocks (deliberately
// mismatched from the desired content, so a naive "already converged"
// read of just the first block cannot mask the duplicate) and asserts
// one real run collapses them into a single block and reports having
// done so, and that a second run then reports ordinary convergence with
// an identical sha256, collapsing is a one-time fixup, not a repeated
// rewrite.
func TestContainer_C12_CollapsesDuplicateManagedBlocks(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-dup-test"
	// This test's subject is the collapse-on-duplicate-blocks logic,
	// not package installation: starting from the package-bearing base
	// (the same one the two monitoring-prompt tests use) keeps step1's
	// apt-get out of its budget and off the pty's timing entirely.
	startC12ContainerFrom(t, name, c12BaseWithPackages(t))
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	if out, _, code := exec_("command -v script"); code != 0 {
		skipUnlessCI(t, "C12 (duplicate blocks): the `script` utility is not available in this environment: %s", out)
	}

	const beginMark = "# >>> agentic-server-supervisor (managed) >>>"
	const endMark = "# <<< agentic-server-supervisor (managed) <<<"

	prep := `set -e
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env
cat > /etc/smartd.conf <<'SMARTD_EOF'
` + beginMark + `
DEVICESCAN -a -o on -S on -n standby,q -W 4,45,55 -m stale@example.com -M exec /usr/share/smartmontools/smartd-runner
` + endMark + `
` + beginMark + `
DEVICESCAN -a -o on -S on -n standby,q -W 4,45,55 -m stale@example.com -M exec /usr/share/smartmontools/smartd-runner
` + endMark + `
SMARTD_EOF`
	if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
		skipUnlessCI(t, "C12 (duplicate blocks): throwaway container prep failed: %s %s", out, errOut)
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

	// step4 now asks before touching smartd.conf at all (main's
	// explicit "no flag, ask, defaulting to no"), so collapsing the
	// duplicate blocks needs a real pty answering yes. A single simple
	// command through the pty (absolute path, no compound "cd &&"),
	// the exact shape the two monitoring-prompt tests below use and
	// confirmed to actually deliver the answer to
	// confirm_monitoring_change's read, a "cd DIR && ./relative"
	// form left the captured output as just the pty's own echo of the
	// piped "y" rather than install.sh's real output.
	runConfirmed := func() (string, string, int) {
		return exec_(`printf 'y\n' | script -qec "bash /work/install.sh --env-file /root/test.env" /tmp/script.log`)
	}

	out1, errOut1, code1 := runConfirmed()
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

	out2, _, _ := runConfirmed()
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

// omvSmartdHeader is the exact header a real OpenMediaVault host stamps
// into /etc/smartd.conf when it generates the file from its own config
// database (captured read-only from a real host, per CLAUDE.md; see
// contracts/runtime.md R5). Deliberately the literal bytes, not a
// paraphrase, file_is_omv_managed matches a substring of this, and a
// test using anything else would not prove the real detection works.
const omvSmartdHeader = `# This file is auto-generated by openmediavault (https://www.openmediavault.org)
# WARNING: Do not edit this file, your changes will get lost.

DEFAULT -a -o on -S on -T permissive -R 5! -R 197! -U 198+ -W 0,0,0 -n standby,q
`

// TestContainer_C12_SmartdOMVManagedSkipsWrite: on a host where
// /etc/smartd.conf carries OpenMediaVault's own auto-generated header,
// step4 must not write to it at all -- not even the managed block --
// because OMV regenerates the file on its own schedule and silently
// discards anything else in it. Verified by byte-for-byte comparison of
// the file's content before and after, not by trusting the summary
// line: a test that only checked the note text would not catch a write
// that happened anyway alongside a note claiming it didn't.
func TestContainer_C12_SmartdOMVManagedSkipsWrite(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-omv-smartd"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env
cat > /etc/smartd.conf <<'SMARTD_EOF'
` + omvSmartdHeader + `SMARTD_EOF`
	if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
		t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
	}
	before, _, _ := exec_("sha256sum /etc/smartd.conf")

	out, errOut, code := exec_("bash /work/install.sh --env-file /root/test.env")
	if code != 0 && code != 75 {
		t.Fatalf("FAIL: install.sh exited %d, want 0 or 75: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "step4 /etc/smartd.conf: skipped") || !strings.Contains(out, "OpenMediaVault") {
		t.Errorf("FAIL: OMV-managed smartd.conf was not reported as skipped for that reason: %s", out)
	}
	if strings.Contains(out, "step4 /etc/smartd.conf: updated") || strings.Contains(out, "collapsed") {
		t.Errorf("FAIL: step4 claims to have written an OMV-managed file: %s", out)
	}
	after, _, _ := exec_("sha256sum /etc/smartd.conf")
	if before != after {
		t.Errorf("FAIL: /etc/smartd.conf's bytes changed even though it carries OMV's auto-generated header (before=%s after=%s)", before, after)
	}
	logPass(t, "PASS C12: OMV-managed smartd.conf is left byte-for-byte untouched")
}

// TestContainer_C12_ZedOMVManagedSkipsWrite: step5 checks zed.rc for the
// same OMV marker and must treat it identically, even though OMV does
// not currently generate that file -- the check is cheap insurance
// against the day it does.
func TestContainer_C12_ZedOMVManagedSkipsWrite(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-omv-zed"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env
mkdir -p /etc/zfs/zed.d
cat > /etc/zfs/zed.d/zed.rc <<'ZED_EOF'
` + omvSmartdHeader + `ZED_EOF`
	if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
		t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
	}
	before, _, _ := exec_("sha256sum /etc/zfs/zed.d/zed.rc")

	out, errOut, code := exec_("bash /work/install.sh --env-file /root/test.env")
	if code != 0 && code != 75 {
		t.Fatalf("FAIL: install.sh exited %d, want 0 or 75: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "step5 /etc/zfs/zed.d/zed.rc: skipped") || !strings.Contains(out, "own configuration") {
		t.Errorf("FAIL: OMV-managed zed.rc was not reported as skipped for that reason: %s", out)
	}
	if strings.Contains(out, "step5 /etc/zfs/zed.d/zed.rc: updated") {
		t.Errorf("FAIL: step5 claims to have written an OMV-managed file: %s", out)
	}
	after, _, _ := exec_("sha256sum /etc/zfs/zed.d/zed.rc")
	if before != after {
		t.Errorf("FAIL: /etc/zfs/zed.d/zed.rc's bytes changed even though it carries OMV's auto-generated header (before=%s after=%s)", before, after)
	}
	logPass(t, "PASS C12: OMV-managed zed.rc is left byte-for-byte untouched")
}

// TestContainer_C12_MonitoringPromptDeclineLeavesFileUntouched: a bare
// Enter at step4's confirm prompt (the documented default) must leave
// /etc/smartd.conf's bytes exactly as they were -- checked by sha256,
// not by the summary line, per main's instruction: an assertion that
// only reads the note text cannot tell "correctly declined" apart from
// "wrote the file anyway and declined to say so".
func TestContainer_C12_MonitoringPromptDeclineLeavesFileUntouched(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-prompt-no"
	// This test's subject is the prompt, not package installation: starting
	// from the package-bearing base keeps a step1 apt-get out of its budget.
	startC12ContainerFrom(t, name, c12BaseWithPackages(t))
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	if out, _, code := exec_("command -v script"); code != 0 {
		skipUnlessCI(t, "C12 (prompt decline): the `script` utility is not available in this environment: %s", out)
	}

	prep := `set -e
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env
printf '# pre-existing operator file, not ours\n' > /etc/smartd.conf`
	if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
		t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
	}
	before, _, _ := exec_("sha256sum /etc/smartd.conf")

	// A bare Enter: printf a lone newline into the pty script allocates.
	out, errOut, code := exec_(`printf '\n' | script -qec "bash /work/install.sh --env-file /root/test.env" /tmp/script.log`)
	if code != 0 && code != 75 {
		t.Fatalf("FAIL: install.sh exited %d, want 0 or 75: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "step4 /etc/smartd.conf: skipped") || !strings.Contains(out, "declined at the prompt") {
		t.Errorf("FAIL: a declined prompt was not reported with the expected reason: %s", out)
	}
	after, _, _ := exec_("sha256sum /etc/smartd.conf")
	if before != after {
		t.Errorf("FAIL: /etc/smartd.conf's bytes changed after a declined ([Enter]=No) confirm prompt (before=%s after=%s)", before, after)
	}
	logPass(t, "PASS C12: declining the monitoring prompt (bare Enter) leaves smartd.conf byte-for-byte untouched")
}

// TestContainer_C12_MonitoringPromptConfirmWritesFile: the mirror case
// -- answering "y" at the same prompt must actually write the managed
// block and restart smartd, proving the gate does not just always say
// no.
func TestContainer_C12_MonitoringPromptConfirmWritesFile(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-prompt-yes"
	// This test's subject is the prompt, not package installation: starting
	// from the package-bearing base keeps a step1 apt-get out of its budget.
	startC12ContainerFrom(t, name, c12BaseWithPackages(t))
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	if out, _, code := exec_("command -v script"); code != 0 {
		skipUnlessCI(t, "C12 (prompt confirm): the `script` utility is not available in this environment: %s", out)
	}

	prep := `set -e
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
	if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
		t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
	}

	out, errOut, code := exec_(`printf 'y\n' | script -qec "bash /work/install.sh --env-file /root/test.env" /tmp/script.log`)
	if code != 0 && code != 75 {
		t.Fatalf("FAIL: install.sh exited %d, want 0 or 75: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "step4 /etc/smartd.conf: updated") {
		t.Errorf("FAIL: confirming the prompt did not write smartd.conf: %s", out)
	}
	content, _, _ := exec_("cat /etc/smartd.conf")
	if !strings.Contains(content, "DEVICESCAN") || !strings.Contains(content, "smartd@mailrise.xyz") {
		t.Errorf("FAIL: /etc/smartd.conf does not contain the expected managed DEVICESCAN line after confirming: %s", content)
	}
	logPass(t, "PASS C12: confirming the monitoring prompt (y) writes smartd.conf")
}

// TestContainer_C12_CheckExitCodeMatchesChanged: install.sh --check must
// exit 0 when the only thing left "undone" is the opt-in monitoring
// change (step4/step5), which defaults to No. This is the exact
// contradiction the VM E2E job (contracts/runtime.md R6) found on a real
// host that no container test had ever caught: a real run (no controlling
// terminal, monitoring declined by default) reported changed=0, converged,
// and the immediately following --check exited 1 anyway. The cause:
// confirm_monitoring_change auto-passes under --check purely so the
// preview machinery has something to render, and that hypothetical "yes"
// was being counted as changed even though a real unattended run would
// never make it. An opt-in change that defaults to No is not drift (R5);
// --check must say so.
func TestContainer_C12_CheckExitCodeMatchesChanged(t *testing.T) {
	name := "sentinel-c12-check-matches-changed"
	// This test's subject is the changed/--check accounting, not package
	// installation: starting from the package-bearing base keeps a step1
	// apt-get out of its budget.
	startC12ContainerFrom(t, name, c12BaseWithPackages(t))
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 90*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
	if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
		t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
	}

	// No pty anywhere in this exec: "no controlling terminal" is the
	// ordinary default for curl | sudo bash, not a special case, and it
	// is what the real host that found this bug actually hit.
	out1, errOut1, code1 := exec_("/work/install.sh --env-file /root/test.env < /dev/null 2>&1")
	if code1 != 0 {
		t.Fatalf("FAIL C12 (check matches changed, run 1): exit=%d, want 0: %s %s", code1, out1, errOut1)
	}
	if !strings.Contains(out1, "no controlling terminal to ask, defaulted to No") {
		t.Fatalf("FAIL C12 (check matches changed, run 1): monitoring was not declined for the expected reason: %s", out1)
	}

	out2, errOut2, code2 := exec_("/work/install.sh --check --env-file /root/test.env < /dev/null 2>&1")
	if !strings.Contains(out2, "step4 /etc/smartd.conf: available, would be updated if enabled (opt-in, defaults to No, not counted as drift)") {
		t.Errorf("FAIL C12 (check matches changed, run 2): --check no longer previews the opt-in action, or the wording drifted: %s", out2)
	}
	if code2 != 0 {
		t.Errorf("FAIL C12 (check matches changed, run 2): --check exit=%d, want 0 (a declined-by-default opt-in must not read as drift on an otherwise converged host): %s %s", code2, out2, errOut2)
	}
	logPass(t, "PASS C12: --check exits 0 on a converged host even though the opt-in monitoring change is still available")
}

// TestContainer_C12_MailCredentialsMissingSkipsAllThreeSteps: msmtp
// package PRESENT, MAILRISE_SMTP_USER/PASS ABSENT. Steps 4 and 5 hand
// their alert mail to msmtp regardless of whether msmtp has anything to
// send with (smartd's `-m` target, ZED_EMAIL_PROG=msmtp), gating only
// on package presence would let them "converge" while pointing a real
// host's SMART and ZFS alerts at an msmtp with no config file at all,
// which is worse than the pre-existing broken-but-present config this
// whole area of the script exists to fix. All three of steps 3/4/5 must
// refuse to write, and the run must exit 78 (required ops input missing
// from --env-file, permanent until a human edits .env, never 75's
// "safe to re-run").
func TestContainer_C12_MailCredentialsMissingSkipsAllThreeSteps(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12 (mail creds missing): cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12-nocreds-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", root+":/work:ro",
		"debian:trixie-slim", "sleep", "600")
	if code != 0 {
		skipUnlessCI(t, "C12 (mail creds missing): could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// msmtp installed for real (network access, matches the rest of the
	// C12 suite's real-apt-get assumption), this is the package-PRESENT
	// case, deliberately distinct from MUST 1's package-absent test.
	// .env is intentionally empty: no MAILRISE_SMTP_USER/PASS at all.
	prep := `set -e
cp -r /work /root/repo
apt-get update -qq
apt-get install -y -qq systemd msmtp msmtp-mta >/dev/null 2>&1 || true
: > /root/repo/.env
chmod +x /root/repo/install.sh
mkdir -p /etc/zfs/zed.d`
	if out, errOut, code := exec_(prep); code != 0 {
		skipUnlessCI(t, "C12 (mail creds missing): throwaway container prep failed: %s %s", out, errOut)
	}
	if out, _, code := exec_("dpkg-query -W -f='${Status}' msmtp 2>/dev/null"); code != 0 || !strings.Contains(out, "install ok installed") {
		skipUnlessCI(t, "C12 (mail creds missing): msmtp did not actually install in this environment: %s", out)
	}

	out1, errOut1, code1 := exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
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
		t.Errorf("FAIL C12 (mail creds missing): /etc/smartd.conf must not gain a managed block, smartd would then point live alerts at an unconfigured msmtp: %s", out)
	}
	if out, _, _ := exec_("test -e /etc/zfs/zed.d/zed.rc && echo exists || echo absent"); strings.TrimSpace(out) != "absent" {
		t.Errorf("FAIL C12 (mail creds missing): /etc/zfs/zed.d/zed.rc must not gain a managed block, ZED_EMAIL_PROG=msmtp would then be unusable: %s", out)
	}
	logPass(t, "PASS C12 (mail credentials missing skips steps 3, 4 and 5, and exits 78)")
}

// TestContainer_C12_ExistingMTANeverDisplaced reproduces, at fixture
// scale, the actual production incident: a host that already has a mail
// transport agent (postfix here, standing in for whatever OpenMediaVault
// itself depends on) must never have it removed by this script.
// msmtp-mta and postfix both provide the virtual package
// mail-transport-agent, so `apt-get install msmtp-mta` on a host that
// already has postfix is exactly the request that made apt resolve the
// conflict by removing postfix (and, on the real host, everything that
// in turn depended on it: openmediavault core and ten of its plugins).
// step1 must detect the existing provider and skip msmtp-mta
// specifically, never touch postfix, while still installing the plain
// msmtp client so step3 can write /etc/msmtprc as usual, since msmtp
// itself does not provide mail-transport-agent and cannot conflict.
func TestContainer_C12_ExistingMTANeverDisplaced(t *testing.T) {
	name := "sentinel-c12-existing-mta"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// zed.rc, because the /etc/msmtprc assertion below is only meaningful on
	// a host that has something to read it. With postfix holding the
	// transport role, smartd mails through postfix and never reaches msmtp;
	// ZED_EMAIL_PROG names the binary directly and is the reader that makes
	// the file worth writing. Without this the test asserted a credential
	// file into existence on a host where nothing could ever open it.
	prep := `set -e
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postfix >/dev/null 2>&1
mkdir -p /etc/zfs/zed.d && printf '#!/bin/sh\n# ZED configuration\n' > /etc/zfs/zed.d/zed.rc
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
	if out, errOut, code := exec_(systemctlOKStub + "\n" + prep); code != 0 {
		skipUnlessCI(t, "C12 (existing MTA): prep failed (postfix install needs network): %s %s", out, errOut)
	}
	if out, _, code := exec_("dpkg-query -W -f='${Status}' postfix 2>/dev/null"); code != 0 || !strings.Contains(out, "install ok installed") {
		skipUnlessCI(t, "C12 (existing MTA): postfix did not actually install in this environment: %s", out)
	}

	out, errOut, code := exec_("bash /work/install.sh --env-file /root/test.env")
	if code != 0 && code != 75 {
		t.Fatalf("FAIL C12 (existing MTA): install.sh exited %d, want 0 or 75: %s %s", code, out, errOut)
	}

	if pkgOut, _, _ := exec_("dpkg-query -W -f='${Status}' postfix 2>/dev/null"); !strings.Contains(pkgOut, "install ok installed") {
		t.Errorf("FAIL C12 (existing MTA): postfix was removed by install.sh, this is the exact production incident: %s (install.sh output: %s)", pkgOut, out)
	}
	if pkgOut, _, code := exec_("dpkg-query -W -f='${Status}' msmtp-mta 2>/dev/null"); code == 0 && strings.Contains(pkgOut, "install ok installed") {
		t.Errorf("FAIL C12 (existing MTA): msmtp-mta was installed despite postfix already providing mail-transport-agent, apt would have had to remove postfix to satisfy this: %s", pkgOut)
	}
	if !strings.Contains(out, "postfix") {
		t.Errorf("FAIL C12 (existing MTA): summary does not name the existing MTA it deferred to: %s", out)
	}
	if pkgOut, _, code := exec_("dpkg-query -W -f='${Status}' msmtp 2>/dev/null"); code != 0 || !strings.Contains(pkgOut, "install ok installed") {
		t.Errorf("FAIL C12 (existing MTA): plain msmtp (the client, not msmtp-mta) should still be installed for step3's own use, it does not provide mail-transport-agent and cannot conflict: %s", pkgOut)
	}
	if out2, _, _ := exec_("test -e /etc/msmtprc && echo exists || echo absent"); strings.TrimSpace(out2) != "exists" {
		t.Errorf("FAIL C12 (existing MTA): /etc/msmtprc should still be written via the msmtp client, mail wiring must not silently go dark just because msmtp-mta was correctly skipped: %s", out2)
	}
	logPass(t, "PASS C12 (existing MTA is never displaced, msmtp client installs alongside it)")
}

// TestContainer_C12_PackagesNarrowWhenMailStepsWontRun: a fresh host
// with no --env-file credentials yet is exactly the shape of the real
// incident's first run, nothing downstream of msmtp (step3's own only
// gate is mail credentials, steps 4/5 gate on the same MAIL_OK) can use
// it, so step1 must not install msmtp or msmtp-mta at all. Installing a
// mail transport for wiring the rest of the script is about to skip
// anyway is exactly the shape of the incident even before any MTA
// conflict enters into it: nothing on the host needed step1 to touch
// mail packages in the first place.
func TestContainer_C12_PackagesNarrowWhenMailStepsWontRun(t *testing.T) {
	name := "sentinel-c12-narrow-packages"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// Deliberately empty: no MAILRISE_SMTP_USER/PASS anywhere, the same
	// "operator has not filled .env in yet" state a first bringup starts
	// from.
	if out, errOut, code := exec_(systemctlOKStub + "\n: > /root/test.env"); code != 0 {
		t.Fatalf("FAIL C12 (narrow packages): prep failed: %s %s", out, errOut)
	}

	out, errOut, code := exec_("bash /work/install.sh --env-file /root/test.env")
	if code != 78 {
		t.Fatalf("FAIL C12 (narrow packages): exit=%d, want 78 (missing MAILRISE_SMTP_USER/PASS): %s %s", code, out, errOut)
	}

	for _, pkg := range []string{"rasdaemon", "lm-sensors"} {
		if pkgOut, _, dpkgCode := exec_("dpkg-query -W -f='${Status}' " + pkg + " 2>/dev/null"); dpkgCode != 0 || !strings.Contains(pkgOut, "install ok installed") {
			skipUnlessCI(t, "C12 (narrow packages): %s did not actually install in this environment (no network reachable?): %s", pkg, pkgOut)
		}
	}
	for _, pkg := range []string{"msmtp", "msmtp-mta"} {
		if pkgOut, _, dpkgCode := exec_("dpkg-query -W -f='${Status}' " + pkg + " 2>/dev/null"); dpkgCode == 0 && strings.Contains(pkgOut, "install ok installed") {
			t.Errorf("FAIL C12 (narrow packages): %s was installed even though no mail step can use it without credentials: %s", pkg, pkgOut)
		}
	}
	if !strings.Contains(out, "step1 packages:") || !strings.Contains(out, "msmtp") {
		t.Errorf("FAIL C12 (narrow packages): summary does not explain that msmtp/msmtp-mta were left out: %s", out)
	}
	logPass(t, "PASS C12 (step1 installs only what the steps that will actually run need)")
}

// TestContainer_C12_NoExistingMTAInstallsMsmtpMta is
// TestContainer_C12_ExistingMTANeverDisplaced's other half: a host with
// no pre-existing mail-transport-agent provider has nothing to
// displace, and msmtp-mta is a legitimate, needed install there (step3
// writes /etc/msmtprc and steps 4/5 want ZED_EMAIL_PROG=msmtp to work).
// Without this case, "step1 never installs msmtp-mta at all" would
// also make the existing-MTA test pass, for the wrong reason, silently
// deleting the feature rather than gating it. Both halves, always.
func TestContainer_C12_NoExistingMTAInstallsMsmtpMta(t *testing.T) {
	name := "sentinel-c12-no-existing-mta"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := systemctlOKStub + "\n" +
		`printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
	if out, errOut, code := exec_(prep); code != 0 {
		t.Fatalf("FAIL C12 (no existing MTA): prep failed: %s %s", out, errOut)
	}
	// Confirm the fixture host genuinely has no MTA yet, otherwise a
	// pass here would prove nothing about the "nothing to displace"
	// case it exists to check.
	if pkgOut, _, code := exec_("dpkg-query -W -f='${Status}' postfix exim4 2>/dev/null"); code == 0 && strings.Contains(pkgOut, "install ok installed") {
		t.Fatalf("FAIL C12 (no existing MTA): fixture host already has an MTA installed, this test's premise does not hold: %s", pkgOut)
	}

	out, errOut, code := exec_("bash /work/install.sh --env-file /root/test.env")
	if code != 0 && code != 75 {
		t.Fatalf("FAIL C12 (no existing MTA): install.sh exited %d, want 0 or 75: %s %s", code, out, errOut)
	}

	if pkgOut, _, dpkgCode := exec_("dpkg-query -W -f='${Status}' msmtp-mta 2>/dev/null"); dpkgCode != 0 || !strings.Contains(pkgOut, "install ok installed") {
		skipUnlessCI(t, "C12 (no existing MTA): msmtp-mta did not actually install in this environment (no network reachable?): %s (install.sh output: %s %s)", pkgOut, out, errOut)
	}
	logPass(t, "PASS C12 (no existing MTA present: msmtp-mta still installs normally)")
}

// TestContainer_C12_EnvOwnerUnmappedUID: step 6 resolving the .env owner
// by NAME (stat -c %U) breaks silently for a uid with no /etc/passwd
// entry, stat prints the literal string "UNKNOWN", `install -o UNKNOWN`
// fails, and without a checked exit status the step reports "updated"
// while writing nothing. C12's own .env is root-owned throughout (uid 0
// always resolves), so nothing else exercises this path, this test
// chown's it to a uid with deliberately NO passwd entry (1000, present
// nowhere in a fresh debian:trixie-slim's /etc/passwd) and asserts
// JOURNAL_GID still lands.
func TestContainer_C12_EnvOwnerUnmappedUID(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12c: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12c-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", root+":/work:ro",
		"debian:trixie-slim", "sleep", "300")
	if code != 0 {
		skipUnlessCI(t, "C12c: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 60*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
cp -r /work /root/repo
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
groupmod -g 7777 systemd-journal 2>/dev/null || groupadd -g 7777 systemd-journal
: > /root/repo/.env
chown 1000:1000 /root/repo/.env
chmod 600 /root/repo/.env
chmod +x /root/repo/install.sh
! getent passwd 1000 >/dev/null 2>&1` // the whole point: uid 1000 must have NO passwd entry
	if out, errOut, code := exec_(prep); code != 0 {
		skipUnlessCI(t, "C12c: throwaway container prep failed (or uid 1000 unexpectedly has a passwd entry in this base image): %s %s", out, errOut)
	}

	out, errOut, _ = exec_("cd /root/repo && ./install.sh --env-file /root/repo/.env")
	envContent, _, _ := exec_("cat /root/repo/.env")
	if !strings.Contains(envContent, "JOURNAL_GID=7777") {
		t.Fatalf("FAIL C12c: JOURNAL_GID=7777 missing from .env after install.sh against an unmapped-uid-owned .env (stdout=%q stderr=%q .env=%q)", out, errOut, envContent)
	}
	ownerAfter, _, _ := exec_("stat -c '%u:%g' /root/repo/.env")
	if strings.TrimSpace(ownerAfter) != "1000:1000" {
		t.Errorf("FAIL C12c: .env owner changed to %q, want preserved 1000:1000", strings.TrimSpace(ownerAfter))
	}
	logPass(t, "PASS C12c (JOURNAL_GID written and owner preserved despite no /etc/passwd entry for the owning uid)")
}

// TestContainer_C12_MsmtpDelivery: step3 of install.sh must produce
// a /etc/msmtprc that REAL msmtp can actually authenticate and deliver
// through, not merely one containing the string "auth on". Uses a real
// msmtp binary, a real SMTP server requiring AUTH, and the exact config
// file the script writes, asserting the stub actually received an
// authenticated delivery, not that install.sh exited 0.
func TestContainer_C12_MsmtpDelivery(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12b: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
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
		"-v", root + ":/work:ro",
		"debian:trixie-slim", "sleep", "300"}
	if _, errOut, code := runCmd(t, 60*time.Second, dockerBin(), runArgs...); code != 0 {
		skipUnlessCI(t, "C12b: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
cp -r /work /root/repo
apt-get update -qq
apt-get install -y -qq systemd msmtp msmtp-mta >/dev/null 2>&1
chmod +x /root/repo/install.sh
printf 'MAILRISE_SMTP_USER=probeuser\nMAILRISE_SMTP_PASS=probepass\n' > /root/repo/.env`
	if out, errOut, code := exec_(prep); code != 0 {
		skipUnlessCI(t, "C12b: throwaway container prep failed (msmtp package unavailable?): %s %s", out, errOut)
	}

	// host.containers.internal (Podman, native) / host.docker.internal
	// (real Docker, needs the --add-host above) both point at the stub
	// listening on this test process's host. Try both; whichever the
	// resolver in THIS environment actually answers is the one that
	// matters.
	for _, alias := range []string{"host.containers.internal", "host.docker.internal"} {
		installCmd := fmt.Sprintf("cd /root/repo && ./install.sh --env-file /root/repo/.env --mailrise-host %s --mailrise-port %d", alias, stub.port())
		if out, errOut, code := exec_(installCmd); code != 0 && code != 75 {
			t.Logf("C12b: install.sh with alias %s exited %d (skipping this alias): %s %s", alias, code, out, errOut)
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
				t.Fatal("FAIL C12b: msmtp delivered without ever authenticating, mailrise enforces SMTP AUTH unconditionally (R4), so an unauthenticated send proves nothing about the real path")
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

// startC12AppriseContainer preps a throwaway container the same way
// startC12Container does, but stops short of any docker/curl stub,
// callers install exactly the fakes their case needs, then run
// install.sh via --env-file (never step0a_layout, so no real network
// fetch is on the critical path, these cases must not depend on
// container network reachability, since they test docker/apprise
// integration logic, not package installation).
func startC12AppriseContainer(t *testing.T, name, envFileBody string) {
	t.Helper()
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		skipUnlessCI(t, "C12 docker/apprise: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", root+":/work:ro",
		"debian:trixie-slim", "sleep", "300")
	if code != 0 {
		skipUnlessCI(t, "C12 docker/apprise: could not start throwaway container: %s", errOut)
	}
	t.Cleanup(func() { runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name) })

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}
	// install.sh's own os-support gate requires both apt-get (already in
	// the base image) and systemctl (needs the systemd package) before
	// it runs any step at all, including the docker preflight these
	// cases exist to test.
	prep := `set -e
cp -r /work /root/repo
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
chmod +x /root/repo/install.sh
cat > /root/repo/.env <<'ENVEOF'
` + envFileBody + `
ENVEOF`
	if out, errOut, code := exec_(prep); code != 0 || !strings.Contains(exec1(t, name, "command -v systemctl"), "systemctl") {
		skipUnlessCI(t, "C12 docker/apprise: throwaway container prep failed (no network for systemd package?): %s %s", out, errOut)
	}
}

func exec1(t *testing.T, name, script string) string {
	t.Helper()
	out, _, _ := runCmd(t, 30*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	return out
}

func c12AppriseExec(t *testing.T, name, script string) (string, string, int) {
	t.Helper()
	return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
}

// TestContainer_C12_DockerPreflightMissingIsWarningNotFatal: R5's
// docker preflight must report a missing docker CLI without making the
// whole run fail, smartd/ZED/msmtp have standalone value on a host
// that never runs containers. It must also make the apprise-seed step
// visibly refuse to claim success, so the run summary can never read as
// "the stack is ready" when it cannot start.
func TestContainer_C12_DockerPreflightMissingIsWarningNotFatal(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-docker-missing"
	startC12AppriseContainer(t, name, "MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\nTELEGRAM_BOT_TOKEN=123456789:SECRETTOK1\nTELEGRAM_CHAT_ID=555")

	out, errOut, code := c12AppriseExec(t, name, "cd /root/repo && ./install.sh --env-file /root/repo/.env")
	if code != 0 && code != 75 {
		t.Fatalf("FAIL: install.sh with no docker on PATH exited %d, want 0 or 75 (docker missing must not introduce a new fatal exit code): %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "docker preflight: docker not found") {
		t.Errorf("FAIL: summary does not report the missing docker CLI: %s", out)
	}
	if !strings.Contains(out, "apprise seed: skipped") || !strings.Contains(out, "docker preflight above") {
		t.Errorf("FAIL: apprise seed step did not visibly refuse to seed when docker is unavailable: %s", out)
	}
	if strings.Contains(out, "apprise seed: registered") {
		t.Errorf("FAIL: summary claims apprise was registered while docker is missing, the stack cannot be running: %s", out)
	}
	logPass(t, "PASS C12 docker preflight: missing docker is a warning, not fatal")
}

// TestContainer_C12_DockerPreflightLegacyComposeOnly: a host with only
// the legacy standalone docker-compose binary (no compose plugin) must
// be told specifically that the plugin is what's missing, R5 requires
// this be distinguished from "docker not found" rather than folded into
// one generic message.
func TestContainer_C12_DockerPreflightLegacyComposeOnly(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-docker-legacy"
	startC12AppriseContainer(t, name, "MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass")

	stub := `cat > /usr/local/bin/docker <<'DOCKEREOF'
#!/bin/sh
if [ "$1" = "info" ]; then exit 0; fi
exit 1
DOCKEREOF
chmod +x /usr/local/bin/docker
cat > /usr/local/bin/docker-compose <<'DCEOF'
#!/bin/sh
exit 0
DCEOF
chmod +x /usr/local/bin/docker-compose`
	if out, errOut, code := c12AppriseExec(t, name, stub); code != 0 {
		t.Fatalf("FAIL: could not install docker/docker-compose stubs: %s %s", out, errOut)
	}

	out, errOut, code := c12AppriseExec(t, name, "cd /root/repo && ./install.sh --env-file /root/repo/.env")
	if code != 0 && code != 75 {
		t.Fatalf("FAIL: install.sh exited %d, want 0 or 75: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "legacy standalone docker-compose") {
		t.Errorf("FAIL: legacy docker-compose-only host was not called out specifically, want a message distinguishing it from docker-not-found: %s", out)
	}
	if strings.Contains(out, "docker preflight: docker not found") {
		t.Errorf("FAIL: docker IS present here (only the plugin is missing), must not be reported as \"not found\": %s", out)
	}
	logPass(t, "PASS C12 docker preflight: legacy docker-compose distinguished from missing docker")
}

// dockerComposeReadyStub is the docker fake shared by the apprise-seed
// success/failure cases below: `docker info` and `docker compose
// version` both succeed, so DOCKER_OK/COMPOSE_OK are set and
// step_apprise_seed actually reaches its curl call, the case this
// whole file exists to catch is a guard that looks reached but never
// runs.
const dockerComposeReadyStub = `cat > /usr/local/bin/docker <<'DOCKEREOF'
#!/bin/sh
if [ "$1" = "info" ]; then exit 0; fi
if [ "$1" = "compose" ] && [ "$2" = "version" ]; then exit 0; fi
exit 1
DOCKEREOF
chmod +x /usr/local/bin/docker`

// systemctlOKStub makes `systemctl enable --now rasdaemon` (step2) and
// `systemctl restart smartd` (step4) both report success unconditionally.
// The throwaway container has no real init system, so both calls fail
// there on every run regardless of anything this file tests, without
// this stub, TestContainer_C12_AppriseSeed204IsFailure's exit-code
// assertion would pass for the wrong reason (step2's unrelated failure
// already sets TRANSIENT_FAIL/exit 75 on its own), proving nothing
// about the apprise-seed code path it exists to check.
const systemctlOKStub = `cat > /usr/local/bin/systemctl <<'SYSTEMCTLEOF'
#!/bin/sh
exit 0
SYSTEMCTLEOF
chmod +x /usr/local/bin/systemctl`

// TestContainer_C12_AppriseSeedRegistersAndRedactsToken: with docker/
// compose ready and a stub apprise-api answering 200, install.sh must
// report the Telegram target as registered, and the bot token must
// never appear anywhere in the run's combined output, the same secret
// discipline the mailrise.conf/msmtprc paths already carry.
func TestContainer_C12_AppriseSeedRegistersAndRedactsToken(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-apprise-ok"
	const token = "123456789:SECRETTOK1REDACTME"
	startC12AppriseContainer(t, name, "MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\nTELEGRAM_BOT_TOKEN="+token+"\nTELEGRAM_CHAT_ID=555")

	curlStub := `cat > /usr/local/bin/curl <<'CURLEOF'
#!/bin/sh
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
case "$*" in
  # The seed identifies the listener before sending the token. In this
  # scenario apprise is up, so its own /status endpoint answers.
  *"/status"*) exit 0 ;;
  *"/add/"*) : > "$out"; printf '%s' "200"; exit 0 ;;
esac
exit 7
CURLEOF
chmod +x /usr/local/bin/curl`
	if out, errOut, code := c12AppriseExec(t, name, dockerComposeReadyStub+"\n"+curlStub+"\n"+systemctlOKStub); code != 0 {
		t.Fatalf("FAIL: could not install docker/curl/systemctl stubs: %s %s", out, errOut)
	}

	// systemctlOKStub removes the throwaway container's own unrelated
	// systemctl failures (step2/step4 both fail there on every run),
	// so a genuine registration success must now exit exactly 0, not
	// "0 or 75" tolerating noise that would mask a regression in this
	// code path specifically.
	out, errOut, code := c12AppriseExec(t, name, "cd /root/repo && ./install.sh --env-file /root/repo/.env")
	if code != 0 {
		t.Fatalf("FAIL: install.sh exited %d, want 0: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "apprise seed: registered") {
		t.Errorf("FAIL: apprise seed success was not reported once docker/compose/apprise are all ready: %s", out)
	}
	if strings.Contains(out, token) || strings.Contains(errOut, token) {
		t.Errorf("FAIL: the bot token leaked into install.sh's output, must never appear in any log line")
	}
	logPass(t, "PASS C12 apprise seed: registers and redacts the token")
}

// TestContainer_C12_AppriseSeed204IsFailure: N.3.1's rule applies here
// too, apprise-api answering 204 means the key was never registered.
// install.sh must report this as a failure, never as success, or the
// operator is told notifications work when they silently do not.
func TestContainer_C12_AppriseSeed204IsFailure(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-apprise-204"
	startC12AppriseContainer(t, name, "MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\nTELEGRAM_BOT_TOKEN=123456789:SECRETTOK1\nTELEGRAM_CHAT_ID=555")

	curlStub := `cat > /usr/local/bin/curl <<'CURLEOF'
#!/bin/sh
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
case "$*" in
  # apprise is up and identifies itself; the 204 below is what this test is
  # actually about, a registration that reports success while storing
  # nothing.
  *"/status"*) exit 0 ;;
  *"/add/"*) printf 'Failed to load Apprise configuration.' > "$out"; printf '%s' "204"; exit 0 ;;
esac
exit 7
CURLEOF
chmod +x /usr/local/bin/curl`
	if out, errOut, code := c12AppriseExec(t, name, dockerComposeReadyStub+"\n"+curlStub+"\n"+systemctlOKStub); code != 0 {
		t.Fatalf("FAIL: could not install docker/curl/systemctl stubs: %s %s", out, errOut)
	}

	// systemctlOKStub removes the throwaway container's own unrelated
	// systemctl failures (step2/step4 fail there on every run),
	// without it, exit 75 would already be guaranteed by environment
	// noise regardless of what step_apprise_seed does, and the
	// assertion below would pass for the wrong reason.
	out, errOut, code := c12AppriseExec(t, name, "cd /root/repo && ./install.sh --env-file /root/repo/.env")
	// apprise ANSWERED and told us the key was not registered, a
	// present failure of the primary notification path, not the
	// "stack may not be up yet" case an unreachable apprise reports.
	// The exit code is what a script or `echo $?` actually reads, so
	// this must be exit 75 specifically, never 0: the note alone being
	// right proves the message is honest, not that anything reading
	// the exit status would find out.
	if code != 75 {
		t.Fatalf("FAIL: install.sh exited %d, want 75, apprise reachable but the seed was rejected (204) must not exit 0: %s %s", code, out, errOut)
	}
	if strings.Contains(out, "apprise seed: registered") {
		t.Errorf("FAIL: a 204 response was reported as a successful registration: %s", out)
	}
	if !strings.Contains(out, "204") || !strings.Contains(out, "NOT registered") {
		t.Errorf("FAIL: the 204 response was not surfaced as the specific known apprise-api silent-failure it is: %s", out)
	}
	// The reason apprise gave has to reach the summary. Three failures in
	// this script were reported as a bare "it did not work" while the
	// explanation sat in a file deleted a line later, each costing a round
	// trip to recover something the run already had in hand.
	if !strings.Contains(out, "Failed to load Apprise configuration") {
		t.Errorf("FAIL: apprise's own response body was discarded instead of reported: %s", out)
	}
	logPass(t, "PASS C12 apprise seed: 204 reported as failure, with apprise's reason")
}

// TestContainer_C12_AppriseSeedDryRunNoNetworkCall: --dry-run must not
// perform the registration at all, proven by a curl stub that touches
// a marker file on ANY invocation; the marker's absence is the only way
// this test can distinguish "the guard was never reached" from "the
// guard correctly declined to run", the exact failure shape this
// project's tests have produced before.
func TestContainer_C12_AppriseSeedDryRunNoNetworkCall(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-apprise-dryrun"
	startC12AppriseContainer(t, name, "MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\nTELEGRAM_BOT_TOKEN=123456789:SECRETTOK1\nTELEGRAM_CHAT_ID=555")

	curlStub := `cat > /usr/local/bin/curl <<'CURLEOF'
#!/bin/sh
touch /root/curl-was-called
exit 1
CURLEOF
chmod +x /usr/local/bin/curl`
	if out, errOut, code := c12AppriseExec(t, name, dockerComposeReadyStub+"\n"+curlStub); code != 0 {
		t.Fatalf("FAIL: could not install docker/curl stubs: %s %s", out, errOut)
	}

	out, errOut, code := c12AppriseExec(t, name, "cd /root/repo && ./install.sh --dry-run --env-file /root/repo/.env")
	if code != 0 {
		t.Fatalf("FAIL: --dry-run exited %d, want 0: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "apprise seed: would register") {
		t.Errorf("FAIL: --dry-run did not preview the pending apprise registration: %s", out)
	}
	called, _, _ := c12AppriseExec(t, name, "test -f /root/curl-was-called && echo yes || echo no")
	if strings.TrimSpace(called) != "no" {
		t.Errorf("FAIL: --dry-run invoked curl against apprise, it must never perform the registration")
	}
	logPass(t, "PASS C12 apprise seed: --dry-run never touches the network")
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
	t.Parallel()
	root := repoRoot(t)
	wf := filepath.Join(root, ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("FAIL C13: reading %s: %v", wf, err)
	}
	text := string(data)
	// The metadata step's `images:` source line is deliberately NOT
	// asserted here: pinning it to today's exact expression (currently
	// steps.image.outputs.name, previously raw github.repository)
	// teaches "update the string" rather than "verify the property",
	// and it is the line that broke this test once already. The
	// property that actually matters, the published tag points at the
	// same lowercased path the digests were pushed to, is proven for
	// real by merge's `imagetools inspect` against the pushed ref, not
	// by string-matching this file.
	for _, want := range []string{
		"type=raw,value=latest",
		"type=sha,format=long",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("FAIL C13: ci.yml missing %q", want)
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

// TestContainer_C12_SensorsDetectIsOptInAndNotDrift: sensors-detect probes
// I2C/SMBus buses and its own documentation warns that can hang or wedge
// hardware, so it is never a side effect of installing a supervisor. It is
// offered only when nothing is reporting, defaults to No, and, like every
// other opt-in here, must not be counted as drift: --check auto-answers
// prompts, and counting an unanswered offer would mean --check could never
// exit 0 on a converged host that simply has no board sensors, the exact
// defect the VM run found in step4/step5.
func TestContainer_C12_SensorsDetectIsOptInAndNotDrift(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-sensors-optin"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// A container has no hwmon chips bound, which is the shape that makes
	// the offer appear at all.
	prep := `printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
	if out, errOut, code := exec_(systemctlOKStub + "\n" + prep); code != 0 {
		t.Fatalf("FAIL C12 (sensors opt-in): prep failed: %s %s", out, errOut)
	}

	out, _, code := exec_("bash /work/install.sh --dry-run --env-file /root/test.env 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (sensors opt-in): --dry-run exit=%d: %s", code, out)
	}

	// Asserted POSITIVELY first, and this is the whole point of it. The
	// original version of this test only checked that certain strings were
	// ABSENT, which an install.sh that had lost the function entirely
	// satisfied perfectly: the call site remained, bash reported
	// "report_sensors: command not found" on stderr, nothing was printed to
	// stdout, and every assertion passed. A test for a feature has to fail
	// when the feature is gone.
	if !strings.Contains(out, "sensors:") {
		t.Fatalf("FAIL C12 (sensors opt-in): no sensors line at all, the step did not run: %s", out)
	}
	if strings.Contains(out, "command not found") {
		t.Fatalf("FAIL C12 (sensors opt-in): install.sh called something that does not exist: %s", out)
	}

	// Never the command itself under a mode that promises to change nothing.
	if strings.Contains(out, "running sensors-detect") {
		t.Errorf("FAIL C12 (sensors opt-in): --dry-run ran sensors-detect, a mode that promises to change nothing must never probe hardware: %s", out)
	}
	if strings.Contains(out, "would offer to run sensors-detect") && !strings.Contains(out, "not counted as drift") {
		t.Errorf("FAIL C12 (sensors opt-in): the offer is previewed without saying it is not drift: %s", out)
	}

	// The drift accounting itself: --check must agree with a converged run
	// rather than counting an offer nobody accepted.
	outCheck, _, codeCheck := exec_("bash /work/install.sh --check --env-file /root/test.env 2>&1")
	if strings.Contains(outCheck, "running sensors-detect") {
		t.Errorf("FAIL C12 (sensors opt-in): --check ran sensors-detect: %s", outCheck)
	}
	if strings.Contains(outCheck, "command not found") {
		t.Errorf("FAIL C12 (sensors opt-in): --check called something that does not exist: %s", outCheck)
	}
	_ = codeCheck

	logPass(t, "PASS C12 (sensors-detect is offered, never run unattended, and never counted as drift)")
}

// TestContainer_C12_MsmtprcSkippedWhenNothingReadsIt: the shape of a real
// OpenMediaVault host. postfix holds mail-transport-agent so msmtp-mta is
// correctly declined, and zed.rc carries OMV's marker so step5 refuses to
// touch it. Nothing is left that opens /etc/msmtprc: smartd hands mail to
// sendmail, which is postfix here, and ZED_EMAIL_PROG is never set. Writing
// it would put the mailrise password in a second place on disk to configure
// a path the host cannot take, and would report mail as wired in the same
// summary where steps 4 and 5 say they configured nothing.
func TestContainer_C12_MsmtprcSkippedWhenNothingReadsIt(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-msmtprc-no-reader"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// postfix, because without an existing mail-transport-agent step1 plans
	// to install msmtp-mta, and a host that is about to get msmtp as its
	// system transport genuinely does have a reader. Declining msmtp-mta is
	// what leaves nothing behind, and that only happens when the role is
	// already taken. This is the shape of the target host, not a contrivance.
	prep := `set -e
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postfix >/dev/null 2>&1
mkdir -p /etc/zfs/zed.d
printf '# This file is auto-generated by openmediavault\n# WARNING: Do not edit this file, your changes will get lost.\n' > /etc/zfs/zed.d/zed.rc
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
	if out, errOut, code := exec_(systemctlOKStub + "\n" + prep); code != 0 {
		skipUnlessCI(t, "C12 (msmtprc no reader): prep failed (postfix install needs network): %s %s", out, errOut)
	}
	if out, _, code := exec_("dpkg-query -W -f='${Status}' postfix 2>/dev/null"); code != 0 || !strings.Contains(out, "install ok installed") {
		skipUnlessCI(t, "C12 (msmtprc no reader): postfix did not actually install in this environment: %s", out)
	}

	out, _, code := exec_("bash /work/install.sh --dry-run --env-file /root/test.env 2>&1")
	if code != 0 {
		t.Fatalf("FAIL C12 (msmtprc no reader): --dry-run exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "step3 /etc/msmtprc: skipped, nothing on this host reads it") {
		t.Errorf("FAIL C12 (msmtprc no reader): step3 did not skip on a host with no msmtp reader: %s", out)
	}
	if strings.Contains(out, "step3 /etc/msmtprc: would be updated") {
		t.Errorf("FAIL C12 (msmtprc no reader): step3 previewed a write nothing would read: %s", out)
	}

	// And the file must genuinely not appear on a real run.
	if _, _, code := exec_("bash /work/install.sh --env-file /root/test.env 2>&1"); code != 0 && code != 75 {
		t.Logf("install.sh exited %d (packages may be unavailable in this environment)", code)
	}
	if fileOut, _, _ := exec_("test -e /etc/msmtprc && echo exists || echo absent"); strings.TrimSpace(fileOut) != "absent" {
		t.Errorf("FAIL C12 (msmtprc no reader): /etc/msmtprc was written on a host where nothing reads it, putting the mailrise password on disk for no delivery path: %s", fileOut)
	}

	logPass(t, "PASS C12 (msmtprc is not written when no msmtp reader exists)")
}

// TestContainer_C12_PlatformNotificationEmail: step7 points the platform's
// own notification email at mailrise, which is the delivery path on a host
// where msmtp has no role at all.
//
// Every branch here was verified once by hand, by mutating install.sh and
// watching the failure. That proves the assertions bite on the day it is
// done and nothing afterwards, which is worth exactly as much as the
// sensors test that passed against a function that did not exist. These
// cover the same ground permanently.
func TestContainer_C12_PlatformNotificationEmail(t *testing.T) {
	t.Parallel()

	const unsetJSON = `{"enable": false, "server": "", "port": 25, "tls": "none", "sender": "", "authentication": {"enable": false, "username": "", "password": ""}, "primaryemail": "", "secondaryemail": ""}`
	const configuredJSON = `{"enable": true, "server": "smtp.example.org", "port": 587, "tls": "starttls", "sender": "nas@example.org", "primaryemail": "admin@example.org", "secondaryemail": ""}`

	// rpcStub answers "get" with the supplied document and records every
	// other call, so a write can be asserted on by what was attempted rather
	// than by trusting the summary line to describe itself honestly.
	rpcStub := func(getJSON string) string {
		// Models omv-rpc's real contract: the bare object on stdout with
		// exit 0 on success, and the {"response":null,"error":{...}} wrapper
		// on STDERR with exit 1 on failure. It also enforces the same role
		// rule the real binary does, granting administrator only to the name
		// in OMV_WEBGUI_ADMINUSER_NAME, which is what caught a hardcoded
		// "admin" being refused as "Invalid context role" on a real host.
		return `mkdir -p /usr/sbin /etc/default
printf 'OMV_WEBGUI_ADMINUSER_NAME="webadmin"\n' > /etc/default/openmediavault
cat > /usr/sbin/omv-rpc <<'STUB'
#!/bin/sh
user=""
if [ "$1" = "-u" ]; then user="$2"; fi
if [ "$user" != "webadmin" ]; then
  echo '{"response":null,"error":{"code":0,"message":"Invalid context role."}}' >&2
  exit 1
fi
for a in "$@"; do last="$a"; done
if [ "$last" = "{}" ]; then
  cat <<'JSON'
` + getJSON + `
JSON
  exit 0
fi
printf '%s\n' "$*" >> /tmp/omv-rpc-writes
exit 0
STUB
chmod +x /usr/sbin/omv-rpc`
	}

	t.Run("previews the offer and does not count it as drift", func(t *testing.T) {
		t.Parallel()
		name := "sentinel-c12-notify-unset"
		startC12Container(t, name)
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		prep := rpcStub(unsetJSON) + "\n" + `printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
		if out, errOut, code := exec_(systemctlOKStub + "\n" + prep); code != 0 {
			t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
		}

		out, _, code := exec_("bash /work/install.sh --dry-run --env-file /root/test.env 2>&1")
		if code != 0 {
			t.Fatalf("FAIL: --dry-run exit=%d: %s", code, out)
		}
		if strings.Contains(out, "command not found") {
			t.Fatalf("FAIL: install.sh called something that does not exist: %s", out)
		}
		if !strings.Contains(out, "would offer to point it at mailrise") {
			t.Errorf("FAIL: the offer was not previewed: %s", out)
		}
		if !strings.Contains(out, "not counted as drift") {
			t.Errorf("FAIL: an opt-in previewed without saying it is not drift is what makes --check unable to exit 0 on a converged host: %s", out)
		}
		if w, _, _ := exec_("cat /tmp/omv-rpc-writes 2>/dev/null || true"); strings.TrimSpace(w) != "" {
			t.Errorf("FAIL: --dry-run wrote through omv-rpc: %s", w)
		}
		logPass(t, "PASS C12 (platform notification email: offered, previewed, not drift)")
	})

	t.Run("never overwrites an existing configuration", func(t *testing.T) {
		t.Parallel()
		name := "sentinel-c12-notify-configured"
		// Same base and stubs as the other prompt test in this file: an
		// interactive run needs docker to look ready and the packages
		// present, or install.sh stops somewhere earlier than step7.
		startC12ContainerFrom(t, name, c12BaseWithPackages(t))
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		if out, _, code := exec_("command -v script"); code != 0 {
			skipUnlessCI(t, "C12 (notify configured): the `script` utility is not available: %s", out)
		}
		prep := rpcStub(configuredJSON) + "\n" + `printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
		if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
			t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
		}

		// Answering yes to everything, which is the hostile case: a host
		// already mailing somewhere must be left alone even then, because
		// silently redirecting every notification it sends to a Telegram bot
		// is the worst thing this script could do.
		out, _, _ := exec_(`printf 'y\ny\ny\ny\n' | script -qec "bash /work/install.sh --env-file /root/test.env" /tmp/s.log`)
		if !strings.Contains(out, "already configured") {
			t.Errorf("FAIL: an existing configuration was not reported as left alone: %s", out)
		}
		if !strings.Contains(out, "smtp.example.org") {
			t.Errorf("FAIL: the summary does not name the server it deferred to: %s", out)
		}
		w, _, _ := exec_("cat /tmp/omv-rpc-writes 2>/dev/null || true")
		if strings.TrimSpace(w) != "" {
			t.Errorf("FAIL: install.sh overwrote a configuration its operator chose, even after answering yes: %s", w)
		}
		logPass(t, "PASS C12 (platform notification email: existing settings survive an unconditional yes)")
	})

	t.Run("writes the right payload when accepted", func(t *testing.T) {
		t.Parallel()
		name := "sentinel-c12-notify-write"
		startC12ContainerFrom(t, name, c12BaseWithPackages(t))
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		if out, _, code := exec_("command -v script"); code != 0 {
			skipUnlessCI(t, "C12 (notify write): the `script` utility is not available: %s", out)
		}
		// A password carrying both characters JSON has to escape. msmtprc
		// mangled one of these once, which is what
		// TestContainer_C12_MsmtprcHostileSecrets exists for; the same value
		// now has to survive a second serialisation.
		prep := rpcStub(unsetJSON) + "\n" + `printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=pa"ss\\word\n' > /root/test.env`
		if out, errOut, code := exec_(dockerComposeReadyStub + "\n" + systemctlOKStub + "\n" + prep); code != 0 {
			t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
		}

		out, _, _ := exec_(`printf 'y\ny\ny\ny\n' | script -qec "bash /work/install.sh --env-file /root/test.env" /tmp/s.log`)
		if !strings.Contains(out, "pointed at mailrise") {
			t.Fatalf("FAIL: accepting the offer did not store the settings: %s", out)
		}
		w, _, _ := exec_("cat /tmp/omv-rpc-writes 2>/dev/null || true")
		if !strings.Contains(w, "EmailNotification set") {
			t.Fatalf("FAIL: no write reached omv-rpc after accepting: %s", w)
		}
		for _, want := range []string{
			`"enable":true`,
			`"server":"127.0.0.1"`,
			`"port":8025`,
			`"tls":"none"`,
			`"authenable":true`,
			`"username":"testuser"`,
			`"primaryemail":"omv@mailrise.xyz"`,
		} {
			if !strings.Contains(w, want) {
				t.Errorf("FAIL: stored payload is missing %s: %s", want, w)
			}
		}
		// The escaping itself: a literal quote and backslash must reach the
		// document escaped, not raw and not doubled into something else.
		if !strings.Contains(w, `"password":"pa\"ss\\word"`) {
			t.Errorf("FAIL: a password containing a quote and a backslash was not escaped correctly for JSON: %s", w)
		}
		// And the password must never be echoed back into the summary.
		if strings.Contains(out, `pa"ss`) {
			t.Errorf("FAIL: the password appeared in install.sh output: %s", out)
		}
		logPass(t, "PASS C12 (platform notification email: accepted write carries a correctly escaped payload)")
	})

	t.Run("does not depend on msmtp being installed", func(t *testing.T) {
		t.Parallel()
		name := "sentinel-c12-notify-no-msmtp"
		startC12Container(t, name)
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		// This is the shape of the target host: the platform holds
		// mail-transport-agent, so install.sh correctly installs no msmtp at
		// all, which drives MAIL_OK to 0. Gating step7 on MAIL_OK would skip
		// it for want of a package it never uses, and report the reason as
		// missing credentials that are sitting in the env file.
		prep := rpcStub(unsetJSON) + "\n" + `printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
		if out, errOut, code := exec_(systemctlOKStub + "\n" + prep); code != 0 {
			t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
		}
		if out, _, code := exec_("dpkg-query -W -f='${Status}' msmtp 2>/dev/null"); code == 0 && strings.Contains(out, "install ok installed") {
			t.Skip("msmtp is installed in this base image, this case needs it absent")
		}

		out, _, _ := exec_("bash /work/install.sh --dry-run --env-file /root/test.env 2>&1")
		if !strings.Contains(out, "would offer to point it at mailrise") {
			t.Errorf("FAIL: step7 did not run with msmtp absent, which is the only shape it exists for: %s", out)
		}
		if strings.Contains(out, "step7 platform notification email: skipped, MAILRISE_SMTP_USER") {
			t.Errorf("FAIL: step7 reported missing credentials that are present in the env file: %s", out)
		}
		logPass(t, "PASS C12 (platform notification email: independent of msmtp)")
	})

	t.Run("reports why omv-rpc refused instead of hiding it", func(t *testing.T) {
		t.Parallel()
		name := "sentinel-c12-notify-refused"
		startC12Container(t, name)
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		// A stub that refuses everything, the way the real binary refuses a
		// caller whose -u name is not the configured web administrator.
		prep := `mkdir -p /usr/sbin && cat > /usr/sbin/omv-rpc <<'STUB'
#!/bin/sh
echo '{"response":null,"error":{"code":0,"message":"Invalid context role."}}' >&2
exit 1
STUB
chmod +x /usr/sbin/omv-rpc
printf 'MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\n' > /root/test.env`
		if out, errOut, code := exec_(systemctlOKStub + "\n" + prep); code != 0 {
			t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
		}

		out, _, _ := exec_("bash /work/install.sh --dry-run --env-file /root/test.env 2>&1")
		if !strings.Contains(out, "omv-rpc refused the read") {
			t.Errorf("FAIL: a refused read was not reported as a refusal: %s", out)
		}
		// The reason has to survive into the summary. The first version of
		// this step discarded stderr and reported every failure identically,
		// which on a real host meant the cause could not be read off the run
		// that hit it.
		if !strings.Contains(out, "Invalid context role") {
			t.Errorf("FAIL: the reason omv-rpc gave was swallowed: %s", out)
		}
		logPass(t, "PASS C12 (platform notification email: a refused read reports why)")
	})
}

// TestContainer_C12_AppriseSeedVerifiesEndpointBeforeSending: a published
// port is first-come on a host running more than one compose project, and
// this one is not reserved. On the target host another project's container
// already held 8000, so the seed posted "tgram://TOKEN/CHAT_ID" to a
// stranger, which answered 401 and may well have logged the body.
//
// The credential must not leave the host until something identifies itself
// as apprise on /status, the endpoint the compose file's own healthcheck
// already uses.
func TestContainer_C12_AppriseSeedVerifiesEndpointBeforeSending(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		statusCode string
		wantSent   bool
		wantPhrase string
	}{
		{
			name:       "no apprise on the port",
			statusCode: "1", // curl -f fails, as it would against a 401 or a stranger
			wantSent:   false,
			wantPhrase: "were NOT sent",
		},
		{
			name:       "apprise answers status",
			statusCode: "0",
			wantSent:   true,
			wantPhrase: "apprise seed",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			name := "sentinel-c12-apprise-verify-" + strings.ReplaceAll(c.name, " ", "-")
			startC12AppriseContainer(t, name, "MAILRISE_SMTP_USER=testuser\nMAILRISE_SMTP_PASS=testpass\nTELEGRAM_BOT_TOKEN=123456789:SECRETTOK1\nTELEGRAM_CHAT_ID=555")

			// The stub separates the two calls the seed makes: the identity
			// probe against /status, and the registration that carries the
			// token. Whatever reaches /add is recorded verbatim, so the test
			// asserts on what actually left the host rather than on the
			// summary's description of it.
			curlStub := `cat > /usr/local/bin/curl <<'CURLEOF'
#!/bin/sh
for a in "$@"; do last="$a"; done
case "$last" in
  */status)
    exit ` + c.statusCode + `
    ;;
esac
cat >> /root/add-body 2>/dev/null
echo "$@" >> /root/add-args
printf '200'
exit 0
CURLEOF
chmod +x /usr/local/bin/curl`
			if out, errOut, code := c12AppriseExec(t, name, dockerComposeReadyStub+"\n"+curlStub); code != 0 {
				t.Fatalf("FAIL: could not install stubs: %s %s", out, errOut)
			}

			out, _, _ := c12AppriseExec(t, name, "cd /root/repo && ./install.sh --env-file /root/repo/.env")

			body, _, _ := c12AppriseExec(t, name, "cat /root/add-body 2>/dev/null || true")
			args, _, _ := c12AppriseExec(t, name, "cat /root/add-args 2>/dev/null || true")
			sent := strings.Contains(body, "SECRETTOK1") || strings.Contains(args, "SECRETTOK1")

			if sent != c.wantSent {
				t.Errorf("FAIL: token sent = %v, want %v. body=%q args=%q summary=%s", sent, c.wantSent, body, args, out)
			}
			if !strings.Contains(out, c.wantPhrase) {
				t.Errorf("FAIL: want %q in the summary: %s", c.wantPhrase, out)
			}
			if !c.wantSent {
				// The reason has to name the port, since the fix is to move
				// off it and nothing else in the output says which one failed.
				if !strings.Contains(out, "APPRISE_PORT") {
					t.Errorf("FAIL: the refusal does not say how to change the port: %s", out)
				}
				// And it must never print the token while explaining itself.
				if strings.Contains(out, "SECRETTOK1") {
					t.Errorf("FAIL: the token appeared in install.sh output: %s", out)
				}
			}
			logPass(t, "PASS C12 (apprise seed verifies the endpoint: %s)", c.name)
		})
	}
}

// TestContainer_C12_AppriseePortDiscovery: apprise's published port is not
// reserved, and on a host running more than one compose project it is
// first-come. On the target host restic-rest-server already held 8000, so a
// hardcoded default meant the container could not bind and the seed posted
// its credentials to whatever had.
//
// Two properties, and the second matters as much as the first: an occupied
// port is skipped, and a port already recorded in the env file is never
// re-picked, so a service appearing later cannot silently move sentinel.
func TestContainer_C12_AppriseePortDiscovery(t *testing.T) {
	t.Parallel()

	t.Run("skips a port something is already listening on", func(t *testing.T) {
		t.Parallel()
		name := "sentinel-c12-port-occupied"
		startC12Container(t, name)
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		if out, _, code := exec_("command -v nc"); code != 0 {
			skipUnlessCI(t, "C12 (port discovery): nc is not available in this image: %s", out)
		}
		// A real listener on 8000, the way restic-rest-server holds it.
		if out, errOut, code := exec_(systemctlOKStub + "\nnohup nc -l -k 127.0.0.1 8000 >/dev/null 2>&1 & sleep 1; echo started"); code != 0 {
			t.Fatalf("FAIL: could not start a listener: %s %s", out, errOut)
		}

		out, _, code := exec_("bash /work/install.sh --dry-run --stack-dir /root/stack 2>&1")
		if code != 0 && code != 75 && code != 78 {
			t.Fatalf("FAIL: --dry-run exit=%d: %s", code, out)
		}
		if !strings.Contains(out, "8000 is already in use") {
			t.Errorf("FAIL: an occupied 8000 was not reported: %s", out)
		}
		if !strings.Contains(out, "using 8001 instead") {
			t.Errorf("FAIL: discovery did not move to the next free port: %s", out)
		}
		logPass(t, "PASS C12 (apprise port discovery: an occupied port is skipped)")
	})

	t.Run("never re-picks a port already recorded", func(t *testing.T) {
		t.Parallel()
		name := "sentinel-c12-port-recorded"
		startC12Container(t, name)
		exec_ := func(script string) (string, string, int) {
			return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
		}
		// An operator's own choice, or an earlier run's. Either way it stands,
		// even though nothing is listening on 8000 and discovery would
		// otherwise have picked it.
		prep := `mkdir -p /root/stack && printf 'APPRISE_PORT=9317\n' > /root/stack/.env`
		if out, errOut, code := exec_(systemctlOKStub + "\n" + prep); code != 0 {
			t.Fatalf("FAIL: prep failed: %s %s", out, errOut)
		}

		out, _, _ := exec_("bash /work/install.sh --dry-run --env-file /root/stack/.env 2>&1")
		if strings.Contains(out, "is already in use") {
			t.Errorf("FAIL: discovery ran even though a port was already recorded: %s", out)
		}
		after, _, _ := exec_("grep '^APPRISE_PORT=' /root/stack/.env")
		if !strings.Contains(after, "9317") {
			t.Errorf("FAIL: a recorded port was overwritten: %s", after)
		}
		logPass(t, "PASS C12 (apprise port discovery: a recorded port is left alone)")
	})
}

// TestContainer_C12_AppriseSeedRejectsMalformedToken: hand-editing the env
// file on the target host left two bot tokens concatenated, so the seed
// posted a URL apprise could not parse and answered 400 for. That is a
// confusing way to learn about a typo, and it puts a malformed credential
// on the wire to find out. The shape is checked first, and nothing is sent.
func TestContainer_C12_AppriseSeedRejectsMalformedToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"two tokens joined", "8759680967:XXXXXaNfG1Ovz8759680967:AAE1bQLaNfG1Ovz", "more than one colon"},
		{"no colon at all", "8759680967AAE1bQLaNfG1Ovz", "has no colon"},
		{"whitespace inside", "8759680967:AAE1b QLaNfG1Ovz", "contains whitespace"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			name := "sentinel-c12-bad-token-" + strings.ReplaceAll(c.name, " ", "-")
			startC12AppriseContainer(t, name, "MAILRISE_SMTP_USER=u\nMAILRISE_SMTP_PASS=p\nTELEGRAM_BOT_TOKEN="+c.token+"\nTELEGRAM_CHAT_ID=555")

			// Records anything that reaches the network, so "nothing was
			// sent" is asserted against what happened rather than against
			// the summary's account of it.
			curlStub := `cat > /usr/local/bin/curl <<'CURLEOF'
#!/bin/sh
for a in "$@"; do last="$a"; done
case "$last" in
  */status) exit 0 ;;
esac
cat >> /root/sent 2>/dev/null
echo "$*" >> /root/sent
printf '200'
exit 0
CURLEOF
chmod +x /usr/local/bin/curl`
			if out, errOut, code := c12AppriseExec(t, name, dockerComposeReadyStub+"\n"+curlStub); code != 0 {
				t.Fatalf("FAIL: could not install stubs: %s %s", out, errOut)
			}

			out, _, _ := c12AppriseExec(t, name, "cd /root/repo && ./install.sh --env-file /root/repo/.env")
			if !strings.Contains(out, c.want) {
				t.Errorf("FAIL: want %q in the summary: %s", c.want, out)
			}
			sent, _, _ := c12AppriseExec(t, name, "cat /root/sent 2>/dev/null || true")
			if strings.Contains(sent, "/add/") {
				t.Errorf("FAIL: a malformed token was sent to apprise anyway: %s", sent)
			}
			logPass(t, "PASS C12 (apprise seed rejects a malformed token: %s)", c.name)
		})
	}
}

// TestContainer_C12_CaptureFixturesScrubsRealIdentifiers runs
// capture-fixtures.sh end to end against stubbed tools that emit a known
// serial in every shape it has ever leaked in, and asserts the serial does
// not survive.
//
// This exists because the scrubber has leaked three times and each fix was
// "verified" by exercising a piece of it in isolation. The last round eval'd
// the scrub function with a hand-built identifier list, which proved the
// substitution works and never noticed that the function producing that list
// was not defined in the script at all. The call site shipped, the error was
// swallowed, and a capture carrying every drive serial reported itself clean.
//
// Running the whole script is the only check that could have caught that, so
// it is the check that exists now.
func TestContainer_C12_CaptureFixturesScrubsRealIdentifiers(t *testing.T) {
	t.Parallel()
	name := "sentinel-c12-capture-fixtures"
	startC12Container(t, name)
	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// One serial, emitted in every shape that has leaked: smartctl's
	// "Serial Number", smartd's "S/N:", a by-id path, and smartd's state
	// FILE NAME, which is the shape the last round missed.
	const serial = "ZVTAQSTVFAKE"
	stubs := `mkdir -p /usr/local/bin
cat > /usr/local/bin/lsblk <<'EOF'
#!/bin/sh
echo ` + serial + `
EOF
cat > /usr/local/bin/smartctl <<'EOF'
#!/bin/sh
case "$*" in
  *--scan*) echo "/dev/sda -d sat" ;;
  *-i*) printf 'Serial Number:    ` + serial + `\nLU WWN Device Id: 5 000c50 0e6d9650a\n' ;;
  *) printf 'Serial Number:    ` + serial + `\nUser Capacity:    1.000.204.886.016 bytes [1,00 TB]\n' ;;
esac
EOF
cat > /usr/local/bin/journalctl <<'EOF'
#!/bin/sh
printf '{"__REALTIME_TIMESTAMP":"1787335231109512","MESSAGE":"Device: /dev/disk/by-id/ata-ST18000NM003D-3DL103_` + serial + ` [SAT], S/N:` + serial + `, state written to /var/lib/smartmontools/smartd.ST18000NM003D_3DL103-` + serial + `.ata.state"}\n'
EOF
cat > /usr/local/bin/sensors <<'EOF'
#!/bin/sh
echo "{}"
EOF
cat > /usr/local/bin/zpool <<'EOF'
#!/bin/sh
echo "  pool: tank"
EOF
cat > /usr/local/bin/ras-mc-ctl <<'EOF'
#!/bin/sh
echo "no errors"
EOF
cat > /usr/local/bin/docker <<'EOF'
#!/bin/sh
echo "[]"
EOF
chmod +x /usr/local/bin/lsblk /usr/local/bin/smartctl /usr/local/bin/journalctl /usr/local/bin/sensors /usr/local/bin/zpool /usr/local/bin/ras-mc-ctl /usr/local/bin/docker
mkdir -p /dev/disk/by-id && : > "/dev/disk/by-id/ata-ST18000NM003D-3DL103_` + serial + `"`
	if out, errOut, code := exec_(stubs); code != 0 {
		t.Fatalf("FAIL: could not install stubs: %s %s", out, errOut)
	}

	out, errOut, code := exec_("bash /work/capture-fixtures.sh /tmp/fx 2>&1")
	combined := out + errOut

	// The failure that shipped: a function called but never defined. bash
	// reports it and carries on, so nothing else notices.
	if strings.Contains(combined, "command not found") {
		t.Fatalf("FAIL: capture-fixtures.sh called something that does not exist: %s", combined)
	}
	if code != 0 {
		t.Fatalf("FAIL: capture-fixtures.sh exited %d: %s", code, combined)
	}
	if !strings.Contains(combined, "hardware identifier(s) will be replaced by value") {
		t.Errorf("FAIL: the script did not report collecting identifiers: %s", combined)
	}

	// The whole point: the serial must not survive, in ANY of the shapes.
	found, _, _ := exec_("grep -rlF " + serial + " /tmp/fx 2>/dev/null || true")
	if strings.TrimSpace(found) != "" {
		leaked, _, _ := exec_("grep -rhoF -m3 " + serial + " /tmp/fx | head -3")
		t.Errorf("FAIL: the serial survived scrubbing in %s (%s)", strings.TrimSpace(found), strings.TrimSpace(leaked))
	}

	// And data that must NOT be destroyed, which earlier versions mangled.
	kept, _, _ := exec_("cat /tmp/fx/smartd-journal.jsonl 2>/dev/null || true")
	if !strings.Contains(kept, "1787335231109512") {
		t.Errorf("FAIL: journal timestamps were clobbered, they are what the parsers read: %s", kept)
	}
	logPass(t, "PASS C12 (capture-fixtures.sh removes real identifiers end to end)")
}
