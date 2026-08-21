// Package notify is the only place in the system that knows what the
// notification service is (ARCHITECTURE §3, design principle 3). It
// renders a report.Report into the apprise JSON payload, sends it (or a
// mailrise SMTP fallback), and offers the ops-only apprise config seeding
// path. It sends; it does not decide and it does not queue, no dedup, no
// rate limiting, no outbox. `state` owns all of that (N.0).
//
// notify never holds a Telegram credential: TELEGRAM_BOT_TOKEN/CHAT_ID are
// never passed to this process. It posts JSON to apprise, which resolves
// the token from its own config volume.
//
// The binding spec is contracts/notify.md.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/logging"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

var (
	ErrInvalidInput = errors.New("invalid report")  // -> 65
	ErrSend         = errors.New("delivery failed") // -> 4
)

// sentinelErr carries its own rendered message (so callers see, e.g.,
// "http 502: ..." verbatim) while still satisfying errors.Is(err, target).
type sentinelErr struct {
	msg    string
	target error
}

func (e *sentinelErr) Error() string        { return e.msg }
func (e *sentinelErr) Is(target error) bool { return target == e.target }

func wrapErr(target error, format string, args ...any) error {
	return &sentinelErr{msg: fmt.Sprintf(format, args...), target: target}
}

// unwrapURLErr strips the request URL out of a *url.Error before it can
// reach a log line or a returned error string (C7: "Never logged: ...
// APPRISE_KEY"). APPRISE_KEY is the last path segment of every apprise
// request, and net/url percent-encodes that segment into Error()'s text,
// a literal string match (redact, below) only catches keys with no
// character Go escapes, which is most keys but not all (a space, a
// non-ASCII character). Never letting the URL into the error at all is
// the root-cause fix: unwrap to the transport's own error, which never
// carries the URL.
func unwrapURLErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// redact strips cfg.AppriseKey out of an error's text before it can reach
// a log line or a returned error string (C7: "Never logged: ... APPRISE_KEY").
// Belt-and-braces alongside unwrapURLErr: on the postApprise/SeedConfig
// transport path the URL is gone before this ever runs, but redact also
// guards the 204 message and sendMail's dial errors, which are not
// *url.Error and so unwrapURLErr does not touch them.
func redact(cfg *config.Config, err error) string {
	msg := err.Error()
	if cfg.AppriseKey == "" {
		return msg
	}
	return strings.ReplaceAll(msg, cfg.AppriseKey, "<APPRISE_KEY>")
}

// logPostFailure is N.3.5: "post failed (http=<code> or transport=<err>)"
// , two distinct log keys, not one generic "error". postApprise's error
// text is always either "http <code>: ..." or "transport: ...", so the
// prefix tells us which key applies; the http case logs just the code,
// matching the contract's http=<code> shape rather than the full body text.
func logPostFailure(logger *slog.Logger, cfg *config.Config, err error) {
	msg := redact(cfg, err)
	if rest, ok := strings.CutPrefix(msg, "http "); ok {
		if code, _, found := strings.Cut(rest, ":"); found {
			logger.Error("post failed", "http", code)
			return
		}
	}
	logger.Error("post failed", "transport", msg)
}

// logWriter lets tests capture the exact C7 lines this package emits; the
// zero value means stderr.
var logWriter io.Writer

func newLogger(level slog.Level) *slog.Logger {
	w := logWriter
	if w == nil {
		w = os.Stderr
	}
	return slog.New(logging.New(w, level)).With("component", "notify")
}

// Validate is the structural check N.2 describes: presence and enum
// membership only, never rune bounds or component enums, that schema run
// already happened upstream in analyze and again in state. A violation
// must be caught before any network call (N.2), so both Run and Send call
// it first.
func Validate(r report.Report) error {
	switch r.Status {
	case "OK", "WATCH", "ALERT":
	default:
		return fmt.Errorf("status: invalid enum value %q", r.Status)
	}
	if r.Headline == "" {
		return errors.New("headline: required")
	}
	if r.Body == "" {
		return errors.New("body: required")
	}
	for i, f := range r.Findings {
		switch f.Severity {
		case "info", "watch", "alert":
		default:
			return fmt.Errorf("findings[%d].severity: invalid enum value %q", i, f.Severity)
		}
		if f.Component == "" {
			return fmt.Errorf("findings[%d].component: required", i)
		}
		if f.Evidence == "" {
			return fmt.Errorf("findings[%d].evidence: required", i)
		}
		if f.Explanation == "" {
			return fmt.Errorf("findings[%d].explanation: required", i)
		}
	}
	return nil
}

// Send renders and delivers one report. smtpFallback selects the mailrise
// second path (N.0.2: a parameter, not a discovery).
func Send(ctx context.Context, cfg *config.Config, r report.Report, smtpFallback bool) error {
	if err := Validate(r); err != nil {
		return wrapErr(ErrInvalidInput, "%v", err)
	}
	payload := BuildPayload(r, cfg)
	logger := newLogger(logging.ParseLevel(cfg.LogLevel))

	if smtpFallback {
		// mailrise requires SMTP AUTH unconditionally (deploy/mailrise.conf);
		// an unauthenticated attempt would only fail at the moment the
		// second path is actually needed, so refuse it up front (N.5).
		if cfg.MailriseUser == "" || cfg.MailrisePass == "" {
			return wrapErr(ErrSend, "smtp fallback unconfigured")
		}
		if err := sendMail(ctx, cfg, payload.Title, BuildHTMLBody(cfg, r)); err != nil {
			logger.Error("post failed", "transport", redact(cfg, err))
			return wrapErr(ErrSend, "%s", redact(cfg, err))
		}
		logger.Info("sent", "status", r.Status, "host", cfg.Hostname, "path", "smtp")
		return nil
	}

	if err := postApprise(ctx, cfg, payload); err != nil {
		logPostFailure(logger, cfg, err)
		return wrapErr(ErrSend, "%s", redact(cfg, err))
	}
	logger.Info("sent", "status", r.Status, "host", cfg.Hostname, "path", "apprise")
	return nil
}

// postApprise is N.3.1: POST ${APPRISE_URL}/notify/${APPRISE_KEY}.
//
// N.3.1: "204 No Content is a FAILURE, despite being 2xx." apprise returns
// 204 when the configuration key is not registered, nothing was sent,
// to anyone, while every log line would say "sent" if 204 were treated
// like every other 2xx. The same rule applies on the retry path: a 204
// must never OutboxAck.
func postApprise(ctx context.Context, cfg *config.Config, payload Payload) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	endpoint := strings.TrimRight(cfg.AppriseURL, "/") + "/notify/" + cfg.AppriseKey
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: cfg.NotifyTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("transport: %w", unwrapURLErr(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		// C7 forbids logging APPRISE_KEY, "apprise key not registered"
		// carries the same operational meaning without naming it.
		return fmt.Errorf("http 204: apprise key not registered, nothing sent")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("http %d: %s", resp.StatusCode, body)
	}
	return nil
}

// Run executes one CLI invocation. args excludes the "notify" subcommand.
func Run(ctx context.Context, cfg *config.Config, args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	stderr := os.Stderr

	for _, a := range args {
		if a == "--help" || a == "-h" {
			printUsage(stdout)
			return 0, nil
		}
	}

	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "render and print the payload, send nothing")
	seedConfig := fs.Bool("seed-config", false, "upload APPRISE_CONFIG_FILE to apprise-api and exit")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}

	if *seedConfig {
		if *dryRun {
			fmt.Fprintln(stderr, "sentinel notify: --seed-config does not combine with --dry-run")
			return 64, nil
		}
		if fs.NArg() > 0 {
			fmt.Fprintln(stderr, "sentinel notify: --seed-config takes no positional argument")
			return 64, nil
		}
		logger := newLogger(logging.ParseLevel(cfg.LogLevel))
		urls, err := SeedConfig(ctx, cfg)
		if err != nil {
			return 4, fmt.Errorf("seed-config: %w", err)
		}
		logger.Info("seeded", "urls", urls)
		return 0, nil
	}

	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "sentinel notify: at most one positional argument (file)")
		return 64, nil
	}

	var raw []byte
	var err error
	if fs.NArg() == 1 {
		raw, err = os.ReadFile(fs.Arg(0))
	} else {
		raw, err = io.ReadAll(stdin)
	}
	if err != nil {
		return 65, fmt.Errorf("read input: %w", err)
	}

	var rep report.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return 65, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := Validate(rep); err != nil {
		return 65, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	if *dryRun {
		payload := BuildPayload(rep, cfg)
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return 1, fmt.Errorf("marshal payload: %w", err)
		}
		if _, err := fmt.Fprintln(stdout, string(b)); err != nil {
			return 1, fmt.Errorf("write stdout: %w", err)
		}
		return 0, nil
	}

	if err := Send(ctx, cfg, rep, false); err != nil {
		if errors.Is(err, ErrSend) {
			return 4, err
		}
		return 65, err
	}
	return 0, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: sentinel notify [--dry-run] [--seed-config] [file]

  --dry-run      render and print the payload to stdout, send nothing
  --seed-config  upload ${APPRISE_CONFIG_FILE} to apprise-api and exit`)
}
