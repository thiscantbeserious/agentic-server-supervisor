// smtpfallback.go: the mailrise SMTP second path (N.5.1). net/smtp only,
// no external dependency, no TLS, mailrise.conf runs tls: off on the LAN.
package notify

import (
	"context"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
)

// plainAuthNoTLS implements SMTP AUTH PLAIN without stdlib smtp.PlainAuth's
// TLS-or-localhost requirement.
// ponytail: mailrise is a LAN-only plaintext listener (mailrise.conf
// tls: off); switch to smtp.PlainAuth over STARTTLS when the listener
// gets a cert.
type plainAuthNoTLS struct {
	identity, username, password string
}

func (a plainAuthNoTLS) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	resp := []byte(a.identity + "\x00" + a.username + "\x00" + a.password)
	return "PLAIN", resp, nil
}

func (a plainAuthNoTLS) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("unexpected server challenge")
	}
	return nil, nil
}

// smtpDialControl is net.Dialer.Control for the SMTP dial: nil in
// production, so the dialer behaves as if the field were unset. A test
// assigns a function that sleeps before the connect to make the dial
// itself consume part of NOTIFY_TIMEOUT, which is the only portable way
// to observe that the dial and the conversation share one deadline
// rather than getting one each.
var smtpDialControl func(network, address string, c syscall.RawConn) error

func sendMail(ctx context.Context, cfg *config.Config, title, htmlBody string) error {
	addr := net.JoinHostPort(cfg.MailriseHost, strconv.Itoa(cfg.MailrisePort))
	// One absolute deadline for the dial and the conversation together.
	// A dial timeout alone leaves the exchange unbounded (ctx is consumed
	// by DialContext), so a server that accepts TCP and never greets
	// would hold this call, the outbox drain and the whole tick. And a
	// second deadline started after the dial would allow 2 x
	// NOTIFY_TIMEOUT per item, which is not the term the liveness window
	// (C4) counts. Sharing one instant makes dial plus every read and
	// write fit inside NOTIFY_TIMEOUT, the same bound the apprise path
	// gets from http.Client.Timeout.
	deadline := time.Now().Add(cfg.NotifyTimeout)
	dialer := net.Dialer{Deadline: deadline, Control: smtpDialControl}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	// ctx was consumed by the dial. A cancelled tick (shutdown gives an
	// active tick five seconds, then cancels) must also end a conversation
	// that is blocked in a read or write, not wait out NOTIFY_TIMEOUT.
	// Moving the deadline to now fails the pending I/O immediately.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	client, err := smtp.NewClient(conn, cfg.MailriseHost)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()

	auth := plainAuthNoTLS{username: cfg.MailriseUser, password: cfg.MailrisePass}
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := client.Mail(cfg.SentinelMailFrom); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := client.Rcpt(cfg.SentinelMailTo); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write([]byte(buildMIME(cfg, title, htmlBody))); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close data: %w", err)
	}
	return client.Quit()
}

// buildMIME is N.5.1 step 4: CRLF-terminated headers, Content-Type
// text/html, then the HTML body (N.3.6), never payload.Body, which is
// markdown that renders only because the JSON payload carries
// format: markdown alongside it. mailrise selects the notification
// format from Content-Type (verified live 2026-08-18), so text/html
// renders bold and monospace on Telegram exactly like the apprise path;
// text/plain would forward the tags as literal text.
func buildMIME(cfg *config.Config, title, htmlBody string) string {
	now := time.Now()
	if !cfg.Now.IsZero() {
		now = cfg.Now
	}
	var b strings.Builder
	b.WriteString("From: " + cfg.SentinelMailFrom + "\r\n")
	b.WriteString("To: " + cfg.SentinelMailTo + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", title) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	return b.String()
}
