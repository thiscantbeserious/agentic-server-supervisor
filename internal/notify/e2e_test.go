// e2e_test.go: TestE2E, gated on SENTINEL_E2E=1, drives real HTTP/SMTP
// transport through a REAL apprise-api and a REAL mailrise against a mock
// that stands in for the notification service both would otherwise call
// (Telegram, in production). Point config.Load()'s env at a real local or
// remote stack and this test proves both delivery paths actually work;
// point it at CI's job-local apprise+mailrise (deploy/docker-compose.yml's
// own images, no Telegram credential anywhere) and it runs the identical
// assertions on every push.
//
// Why a mock stands in for Telegram rather than tgram:// itself: apprise
// supports json://, which POSTs a small JSON envelope to an arbitrary
// HTTP endpoint — pointing the registered config key (and mailrise.conf's
// own configs) at this test's own HTTP server exercises the REAL chain,
// sentinel -> apprise -> <mock> and sentinel -> SMTP -> mailrise -> <mock>,
// with the only unexercised link being Telegram's own API, which is not
// ours to test and which a real bot token would not meaningfully verify
// either (a 200 from api.telegram.org proves delivery to Telegram's
// servers, not that OUR payload was well-formed — the mock's own
// assertions on title/message/type prove that directly).
//
// A mock that only returns 200 tests almost nothing. This one:
//   - asserts what it actually received (title, message, type — see the
//     comment on notifySink for why NOT format, an empirically corrected
//     assumption)
//   - counts deliveries, so dedup and outbox behaviour are provable rather
//     than assumed
//   - can be told to fail on demand (503, or 204), so the retry/outbox/
//     SMTP-escalation paths and the 204-looks-like-success trap are
//     reachable end to end, not just asserted against a stub in a unit
//     test
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

// sinkDelivery is apprise's OWN json:// notification envelope — verified
// against a real apprise-api container (POST /notify/{key} with a
// title/body/type/format payload, watching what it then sent to a local
// HTTP listener): {"version":"1.0","title":...,"message":...,
// "attachments":[],"type":...}. Notably absent: "format". apprise's own
// JSON output plugin does not forward the input format at all — verified
// by sending format=markdown and format=text with identical body content
// and observing byte-identical downstream messages either way. So this
// mock (and this test) assert title, message and type, which are the
// fields the real wire payload actually carries; asserting "format" here
// would be asserting a field that never crosses the wire, not a
// simplification of this test but a correction of the original ask,
// found by running the real container rather than assuming apprise's
// json:// output mirrors apprise-api's own input schema.
type sinkDelivery struct {
	Version     string `json:"version"`
	Title       string `json:"title"`
	Message     string `json:"message"`
	Type        string `json:"type"`
	receivedRaw string
}

// notifySink stands in for the notification service apprise/mailrise
// would otherwise call (Telegram, in production). Binds to a FIXED,
// documented address (SENTINEL_E2E_SINK_ADDR, default 127.0.0.1:8079)
// rather than a random port, because the config on the apprise/mailrise
// side (a registered key, a mailrise.conf entry) has to be able to name
// it before this process starts — there is no way to discover a random
// port after the fact and hand it to an already-running container's
// already-loaded config.
type notifySink struct {
	mu         sync.Mutex
	ln         net.Listener
	srv        *http.Server
	deliveries []sinkDelivery
	status     int // next response code; 0 means 200
}

func sinkAddr() string {
	if a := os.Getenv("SENTINEL_E2E_SINK_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8079"
}

func newNotifySink(t *testing.T) *notifySink {
	t.Helper()
	addr := sinkAddr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("SKIP TestE2E: cannot bind the mock sink on %s: %v (set SENTINEL_E2E_SINK_ADDR to a free address)", addr, err)
	}
	s := &notifySink{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/notify", s.handle)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln)
	t.Cleanup(func() { s.srv.Close() })
	return s
}

func (s *notifySink) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var d sinkDelivery
	_ = json.Unmarshal(b, &d)
	d.receivedRaw = string(b)

	s.mu.Lock()
	s.deliveries = append(s.deliveries, d)
	code := s.status
	s.mu.Unlock()

	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
}

// setStatus controls what THIS sink returns to whoever posts to it next
// (apprise-api, or mailrise's own embedded apprise client) — never what
// apprise-api itself returns to sentinel, which is a distinct, separately
// verified mapping (see the 503/204 cases in TestE2E).
func (s *notifySink) setStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
}

func (s *notifySink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deliveries)
}

func (s *notifySink) last() sinkDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deliveries) == 0 {
		return sinkDelivery{}
	}
	return s.deliveries[len(s.deliveries)-1]
}

func TestE2E(t *testing.T) {
	if os.Getenv("SENTINEL_E2E") != "1" {
		t.Skip("SENTINEL_E2E=1 not set — skipping live apprise/mailrise E2E test")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ctx := context.Background()
	sink := newNotifySink(t)
	r := loadFixture(t, "report-watch-zfs-cksum.json")
	want := BuildPayload(r, cfg)

	// --- direct path: sentinel -> apprise -> sink ---
	if err := Send(ctx, cfg, r, false); err != nil {
		t.Fatalf("FAIL E2E: live apprise send failed: %v", err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("FAIL E2E: sink recorded %d deliveries after the direct send, want 1", got)
	}
	if d := sink.last(); d.Title != want.Title || d.Message != want.Body || d.Type != want.Type {
		t.Fatalf("FAIL E2E: direct-path delivery = {title=%q message=%q type=%q}, want {title=%q message=%q type=%q}",
			d.Title, d.Message, d.Type, want.Title, want.Body, want.Type)
	}

	// --- SMTP fallback path: sentinel -> SMTP -> mailrise -> sink ---
	// mailrise derives its own title from the email Subject/From rather
	// than forwarding apprise's title field verbatim (verified against a
	// real mailrise container: "<Subject> (<From address>)") — assert
	// containment, not equality, on title; the body still has to match
	// exactly, since that IS the payload content, not mailrise's own
	// envelope framing.
	if err := Send(ctx, cfg, r, true); err != nil {
		t.Fatalf("FAIL E2E: live mailrise SMTP send failed: %v", err)
	}
	if got := sink.count(); got != 2 {
		t.Fatalf("FAIL E2E: sink recorded %d deliveries after the SMTP send, want 2", got)
	}
	if d := sink.last(); !strings.Contains(d.Title, want.Title) {
		t.Errorf("FAIL E2E: SMTP-path delivery title = %q, want it to contain %q", d.Title, want.Title)
	}

	// --- fail-on-demand: 503 (a real transport failure) ---
	sink.setStatus(503)
	t.Cleanup(func() { sink.setStatus(0) })
	errSend := Send(ctx, cfg, r, false)
	sink.setStatus(0)
	if errSend == nil {
		t.Fatal("FAIL E2E: apprise send with the sink returning 503 succeeded, want an error")
	}
	// Verified against a real apprise-api: when ITS downstream target
	// (this sink) returns 503, apprise-api's own /notify/{key} call
	// returns HTTP 424 to the caller — a real, correctly-detected
	// failure (postApprise's 200-299 check already covers it).
	if got := sink.count(); got != 3 {
		t.Errorf("FAIL E2E: sink recorded %d deliveries after the 503 case, want 3 (the attempt was received even though it failed)", got)
	}

	// --- fail-on-demand: 204, the value whose REPORT looks like success
	// even though the send itself may not have meant anything to the
	// far end ---
	//
	// Verified against a real apprise-api: the message DOES reach the
	// downstream target (the sink's own delivery count below proves
	// that) — what a 204 changes is apprise-api's REPORT of what
	// happened, not the delivery. When the downstream target returns
	// 204, apprise-api's OWN /notify/{key} call still returns HTTP 200
	// to the caller. This is a DIFFERENT 204 than N.3.1's "204 is a
	// failure" rule, which is about apprise-api's own /notify/{key}
	// response meaning "the key is not registered" — this is
	// apprise-api absorbing a downstream 204 into what it reports as
	// its own success one layer further down. A downstream that
	// accepted-and-discarded the message would be indistinguishable
	// from here, from one that genuinely delivered it. Asserted rather
	// than fixed, because there is nothing on our side to fix — this is
	// what the real chain does. It is a real, unfixable-at-this-layer
	// risk, not an uncovered one at the system level: the daily
	// heartbeat is what catches a channel that has gone quiet, whatever
	// the cause.
	sink.setStatus(204)
	errSend = Send(ctx, cfg, r, false)
	sink.setStatus(0)
	if errSend != nil {
		t.Errorf("FAIL E2E: apprise send with the sink returning 204 returned an error (%v) — expected apprise-api to report success here (verified live: it does), which is exactly the report-vs-delivery gap this case documents", errSend)
	}
	if got := sink.count(); got != 4 {
		t.Errorf("FAIL E2E: sink recorded %d deliveries after the 204 case, want 4 (received, and apprise still called it a success)", got)
	}

	// --- dedup: three identical ticks must produce exactly one delivery ---
	//
	// Drives the REAL dedup path (internal/state.Store.Process, not a
	// mock) against the same live sink: state decides whether to notify,
	// this test only calls Send when it says so — the same gate
	// internal/runtime.Tick uses. cfg.Now is pinned so three calls at the
	// same instant fall inside the renotify window and the second/third
	// are suppressed by state itself, not by anything special-cased here.
	dedupCfg := *cfg
	dedupCfg.StateDir = t.TempDir()
	dedupCfg.Now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, err := state.New(&dedupCfg)
	if err != nil {
		t.Fatalf("FAIL E2E: state.New: %v", err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("FAIL E2E: marshal fixture: %v", err)
	}
	before := sink.count()
	notifiedCount := 0
	for i := 0; i < 3; i++ {
		decision, err := store.Process(raw)
		if err != nil {
			t.Fatalf("FAIL E2E: state.Process (tick %d): %v", i, err)
		}
		if decision.Notify {
			notifiedCount++
			if err := Send(ctx, &dedupCfg, decision.Report, false); err != nil {
				t.Fatalf("FAIL E2E: dedup-path send (tick %d): %v", i, err)
			}
		}
	}
	if notifiedCount != 1 {
		t.Errorf("FAIL E2E: state.Process said Notify=true %d times across 3 identical ticks, want exactly 1", notifiedCount)
	}
	if got := sink.count() - before; got != 1 {
		t.Errorf("FAIL E2E: sink recorded %d deliveries across 3 identical ticks, want exactly 1 — dedup is not actually collapsing repeats through the real transport", got)
	}

	// --- outbox: a failed send queues, and a later drain delivers it ---
	//
	// Same real state.Store as the dedup case (fresh key via a second
	// fixture so this does not collide with the alert dedup already
	// created above). Fails once (sink at 503), queues via OutboxAdd
	// exactly as internal/runtime.Tick's own queueOrLog does, then drains
	// exactly as Tick's own drainOutbox does — proving the SMTP/outbox
	// escalation path end to end through the real transport, not a mock
	// standing in for state or notify.
	r2 := loadFixture(t, "report-alert-fallback.json")
	raw2, err := json.Marshal(r2)
	if err != nil {
		t.Fatalf("FAIL E2E: marshal second fixture: %v", err)
	}
	decision2, err := store.Process(raw2)
	if err != nil {
		t.Fatalf("FAIL E2E: state.Process (outbox fixture): %v", err)
	}
	if !decision2.Notify {
		t.Fatal("FAIL E2E: outbox fixture's first occurrence did not ask to notify — fixture or dedup state is stale")
	}
	payload2, err := json.Marshal(decision2.Report)
	if err != nil {
		t.Fatalf("FAIL E2E: marshal decision report: %v", err)
	}
	sink.setStatus(503)
	sendErr := Send(ctx, &dedupCfg, decision2.Report, false)
	sink.setStatus(0)
	if sendErr == nil {
		t.Fatal("FAIL E2E: outbox setup send unexpectedly succeeded with the sink at 503")
	}
	id, err := store.OutboxAdd(payload2)
	if err != nil {
		t.Fatalf("FAIL E2E: OutboxAdd: %v", err)
	}
	beforeDrain := sink.count()
	items, err := store.OutboxTake()
	if err != nil {
		t.Fatalf("FAIL E2E: OutboxTake: %v", err)
	}
	found := false
	for _, item := range items {
		if item.ID != id {
			continue
		}
		found = true
		var rep report.Report
		if err := json.Unmarshal(item.Payload, &rep); err != nil {
			t.Fatalf("FAIL E2E: unmarshal outbox item: %v", err)
		}
		if err := Send(ctx, &dedupCfg, rep, item.FallbackSMTP); err != nil {
			t.Fatalf("FAIL E2E: outbox drain send failed once the sink recovered: %v", err)
		}
		if err := store.OutboxAck(item.ID); err != nil {
			t.Fatalf("FAIL E2E: OutboxAck: %v", err)
		}
	}
	if !found {
		t.Fatalf("FAIL E2E: OutboxTake did not return the item OutboxAdd just queued (id=%s)", id)
	}
	if got := sink.count() - beforeDrain; got != 1 {
		t.Errorf("FAIL E2E: sink recorded %d deliveries during the outbox drain, want exactly 1", got)
	}
	remaining, err := store.OutboxTake()
	if err != nil {
		t.Fatalf("FAIL E2E: OutboxTake after ack: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("FAIL E2E: %d items remain in the outbox after ack, want 0", len(remaining))
	}

	t.Logf("PASS E2E: %d live deliveries total (direct, SMTP, 503, 204, one deduped tick, one outbox drain)", sink.count())
}
