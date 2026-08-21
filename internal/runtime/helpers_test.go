package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/notify"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

// --- apprise recorder ---

type appriseRecorder struct {
	mu       sync.Mutex
	status   int
	requests []recordedRequest
	srv      *httptest.Server
}

type recordedRequest struct {
	path string
	body []byte
	at   time.Time
}

func newAppriseRecorder(t *testing.T, status int) *appriseRecorder {
	t.Helper()
	r := &appriseRecorder{status: status}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		buf := make([]byte, req.ContentLength)
		req.Body.Read(buf)
		r.mu.Lock()
		r.requests = append(r.requests, recordedRequest{path: req.URL.Path, body: buf, at: time.Now()})
		st := r.status
		r.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *appriseRecorder) setStatus(s int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = s
}

func (r *appriseRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *appriseRecorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

// --- config ---

func testConfig(t *testing.T, tick time.Time) *config.Config {
	t.Helper()
	stateDir := t.TempDir()
	hostProc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostProc, "uptime"), []byte("1234.5 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	journalDir := t.TempDir()
	// preflight requires at least one non-empty journal dir; a placeholder
	// keeps that true even for tests that never touch journalctl.
	if err := os.WriteFile(filepath.Join(journalDir, ".keep"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOST_PROC", hostProc)
	t.Setenv("HOST_JOURNAL_DIR", journalDir)
	t.Setenv("HOST_JOURNAL_VOLATILE_DIR", t.TempDir())
	t.Setenv("HOST_ROOT", t.TempDir())
	t.Setenv("HOST_RASDAEMON", t.TempDir())
	t.Setenv("AGY_HOME", t.TempDir())
	t.Setenv("AGY_SECRET_DIR", t.TempDir())
	t.Setenv("SENTINEL_HOSTNAME", "bam")
	t.Setenv("APPRISE_URL", "http://127.0.0.1:1") // overridden per-test where notify matters
	t.Setenv("MAILRISE_USER", "u")
	t.Setenv("MAILRISE_PASS", "p")
	t.Setenv("SENTINEL_NOW", tick.UTC().Format(time.RFC3339))
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func newStore(t *testing.T, cfg *config.Config) *state.Store {
	t.Helper()
	s, err := state.New(cfg)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	return s
}

// baseDeps wires a REAL state.Store and REAL notify.Send (pointed at an
// httptest recorder via cfg.AppriseURL), the R8 default for every test:
// "collect/analyze/state/notify are injected through Deps; apprise is an
// httptest.Server recorder". Callers overwrite CollectRun/AnalyzeRun per
// case.
func baseDeps(store *state.Store) Deps {
	return Deps{
		StateProcess: store.Process,
		OutboxAdd:    store.OutboxAdd,
		OutboxTake:   store.OutboxTake,
		OutboxAck:    store.OutboxAck,
		NotifySend:   notify.Send,
	}
}

// --- facts fixtures ---

func factsClean() *facts.Facts {
	return &facts.Facts{
		Meta:   facts.Meta{SchemaVersion: facts.SchemaVersion, Hostname: "bam", CollectorErrors: []facts.CollectorError{}},
		Kernel: &facts.Section[facts.KernelData]{Data: facts.KernelData{Entries: []facts.Entry{}}},
	}
}

func factsWithKernelEntries(entries []facts.Entry) *facts.Facts {
	return &facts.Facts{
		Meta:   facts.Meta{SchemaVersion: facts.SchemaVersion, Hostname: "bam", CollectorErrors: []facts.CollectorError{}},
		Kernel: &facts.Section[facts.KernelData]{Data: facts.KernelData{Count: len(entries), Entries: entries}},
	}
}

func factsKernelSectionError(reason string) *facts.Facts {
	return &facts.Facts{
		Meta:   facts.Meta{SchemaVersion: facts.SchemaVersion, Hostname: "bam", CollectorErrors: []facts.CollectorError{}},
		Kernel: &facts.Section[facts.KernelData]{Err: reason},
	}
}

func critEntry(ts, msg string) facts.Entry {
	return facts.Entry{TS: ts, Priority: 2, Identifier: "kernel", Message: msg}
}

// --- analyze stubs ---

// stubAnalyzeReturning always returns a deep copy of rep, nil error, with
// meta stamped from Options like the real analyze.Run does.
func stubAnalyzeReturning(rep report.Report) func(ctx context.Context, o analyze.Options, d analyze.Deps) (*report.Report, error) {
	return func(ctx context.Context, o analyze.Options, d analyze.Deps) (*report.Report, error) {
		r := rep
		r.Meta = &report.Meta{Hostname: o.Cfg.Hostname, TickSeq: o.Seq}
		return &r, nil
	}
}

func okReport() report.Report {
	return report.Report{
		Status: "OK", Headline: "All systems normal", Body: "Nothing to report.",
		Findings: []report.Finding{}, Resolved: []string{},
	}
}

func watchReport() report.Report {
	return report.Report{
		Status: "WATCH", Headline: "Something to watch", Body: "body",
		Findings: []report.Finding{{Severity: "watch", Component: "zfs", Evidence: "ev", Explanation: "exp"}},
		Resolved: []string{},
	}
}
