package runtime

// outbox_smtp_test.go closes the obligation carried in from T6 (PR #8):
// "the outbox SMTP escalation end-to-end (OUTBOX_SMTP_AFTER failed ticks
// ⇒ tick calls notify.Send(..., smtpFallback=true))... probed correct by
// the reviewer but has no test." R3.2/R3.8 describe the wiring
// (drainOutbox reads OutboxTake's FallbackSMTP and forwards it to
// NotifySend) but nothing in R8's table drives it through 4 REAL failing
// ticks, the gap this file closes.
//
// The SMTP stub mirrors internal/notify's own (a net.Listen goroutine
// speaking the real protocol, C9) rather than importing it, since it is
// unexported there and this package needs its own copy anyway to assert
// on what the stub actually received.

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/collect"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
)

type smtpStub struct {
	mu         sync.Mutex
	sawAuth    bool
	rcptTo     string
	dataText   string
	deliveries int
	ln         net.Listener
}

func newSMTPStub(t *testing.T) *smtpStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpStub{ln: ln}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *smtpStub) hostPort() (string, int) {
	host, portStr, _ := net.SplitHostPort(s.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func (s *smtpStub) snapshot() (deliveries int, sawAuth bool, rcptTo, dataText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries, s.sawAuth, s.rcptTo, s.dataText
}

func (s *smtpStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *smtpStub) handle(conn net.Conn) {
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
			write("250 AUTH PLAIN")
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.sawAuth = true
			s.mu.Unlock()
			write("235 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"):
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			s.mu.Lock()
			s.rcptTo = line
			s.mu.Unlock()
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

// TestTick_OutboxSMTPEscalation_E2E drives 4 REAL ticks through Tick():
// apprise is permanently down (503 on every POST), so a report queued to
// the outbox can only ever be retried via the drain (R3.2 step 5). Per
// R3.2's own documented consequence ("OutboxTake increments attempts on
// every take ... a tick that fails advances the counter twice, once for
// the immediate drain of the payload it just queued, once on the next
// tick"), OUTBOX_SMTP_AFTER=3 (the C3 default) is reached on the THIRD
// drain (tick 3), at which point drainOutbox must call
// notify.Send(..., smtpFallback=true) and deliver via the real SMTP
// protocol to mailrise instead of apprise. Tick 4 proves the outbox is
// then empty and quiescent (acked, not retried again).
func TestTick_OutboxSMTPEscalation_E2E(t *testing.T) {
	cfg := testConfig(t, tick0)
	apprise := newAppriseRecorder(t, 503) // apprise permanently down
	cfg.AppriseURL = apprise.srv.URL
	smtp := newSMTPStub(t)
	cfg.MailriseHost, cfg.MailrisePort = smtp.hostPort()
	cfg.MailriseUser = "u"
	cfg.MailrisePass = "p"
	store := newStore(t, cfg)

	// Seed today's heartbeat so the daily-heartbeat step never fires its
	// own extra notification and confuses which outbox entry is being
	// retried (same isolation TestTick_DrainFailureContributesToExitCode
	// relies on).
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "heartbeat"), []byte("2026-08-15\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := baseDeps(store)
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
	d.AnalyzeRun = stubAnalyzeReturning(okReport()) // this tick's own report never notifies

	// Seed exactly one ALERT payload directly into the outbox, so every
	// drain below retries THIS item and nothing this tick's own (OK,
	// no-findings) report produces.
	if _, err := store.OutboxAdd([]byte(`{"status":"ALERT","headline":"h","body":"b","findings":[],"resolved":[]}`)); err != nil {
		t.Fatal(err)
	}

	outboxCount := func() int {
		entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox"))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}

	// Tick 1: drain takes it, attempts -> 1 (< OutboxSMTPAfter=3), retries
	// via apprise (503), stays queued.
	res1 := Tick(context.Background(), cfg, 1, d)
	if res1.Queued || res1.Notified {
		t.Fatalf("setup guard: tick 1's OWN report must send nothing (Queued=%v Notified=%v)", res1.Queued, res1.Notified)
	}
	if res1.ExitCode != 4 {
		t.Errorf("tick 1 ExitCode = %d, want 4 (drain retry failed via apprise)", res1.ExitCode)
	}
	if n := outboxCount(); n != 1 {
		t.Fatalf("after tick 1: outbox has %d entries, want 1 (still queued)", n)
	}
	if d, _, _, _ := smtp.snapshot(); d != 0 {
		t.Fatalf("after tick 1: smtp stub received %d deliveries, want 0 (fallback not yet due)", d)
	}

	// Tick 2: attempts -> 2 (< 3), still apprise, still fails.
	cfg.Now = tick0.Add(1 * time.Minute)
	res2 := Tick(context.Background(), cfg, 2, d)
	if res2.ExitCode != 4 {
		t.Errorf("tick 2 ExitCode = %d, want 4", res2.ExitCode)
	}
	if n := outboxCount(); n != 1 {
		t.Fatalf("after tick 2: outbox has %d entries, want 1 (still queued)", n)
	}
	if d, _, _, _ := smtp.snapshot(); d != 0 {
		t.Fatalf("after tick 2: smtp stub received %d deliveries, want 0", d)
	}

	// Tick 3: attempts -> 3 (>= OutboxSMTPAfter=3), drainOutbox must call
	// notify.Send(..., smtpFallback=true), which delivers via the real
	// SMTP stub, and OutboxAck the item.
	cfg.Now = tick0.Add(2 * time.Minute)
	res3 := Tick(context.Background(), cfg, 3, d)
	if res3.ExitCode != 0 {
		t.Errorf("tick 3 ExitCode = %d, want 0 (the fallback delivery must succeed)", res3.ExitCode)
	}
	if n := outboxCount(); n != 0 {
		t.Fatalf("after tick 3: outbox has %d entries, want 0 (acked after the SMTP fallback succeeded)", n)
	}
	deliveries, sawAuth, rcptTo, dataText := smtp.snapshot()
	if deliveries != 1 {
		t.Fatalf("after tick 3: smtp stub received %d deliveries, want exactly 1", deliveries)
	}
	if !sawAuth {
		t.Error("smtp stub never saw AUTH, mailrise requires SMTP AUTH unconditionally (N.5)")
	}
	if !strings.Contains(rcptTo, cfg.SentinelMailTo) {
		t.Errorf("RCPT TO = %q, want it to contain %q", rcptTo, cfg.SentinelMailTo)
	}
	if !strings.Contains(dataText, "h") { // the seeded payload's headline
		t.Errorf("DATA did not carry the queued report's content: %q", dataText)
	}
	if apCount := apprise.count(); apCount != 2 {
		t.Errorf("apprise recorder received %d requests, want exactly 2 (tick 1 + tick 2 drains only, tick 3 must go via SMTP, not apprise)", apCount)
	}

	// Tick 4: outbox is empty, the drain must have nothing left to
	// retry, and no further SMTP or apprise traffic.
	cfg.Now = tick0.Add(3 * time.Minute)
	res4 := Tick(context.Background(), cfg, 4, d)
	if res4.ExitCode != 0 {
		t.Errorf("tick 4 ExitCode = %d, want 0 (nothing left to retry)", res4.ExitCode)
	}
	if n := outboxCount(); n != 0 {
		t.Errorf("after tick 4: outbox has %d entries, want 0", n)
	}
	if d, _, _, _ := smtp.snapshot(); d != 1 {
		t.Errorf("after tick 4: smtp stub received %d deliveries total, want still 1 (no re-send of an already-acked item)", d)
	}
	if apCount := apprise.count(); apCount != 2 {
		t.Errorf("after tick 4: apprise recorder received %d requests total, want still 2", apCount)
	}
}
