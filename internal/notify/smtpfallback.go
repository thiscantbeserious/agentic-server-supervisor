// smtpfallback.go: the mailrise SMTP second path (N.5.1). net/smtp only,
// no external dependency, no TLS — mailrise.conf runs tls: off on the LAN.
package notify

import (
	"context"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
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

func sendMail(ctx context.Context, cfg *config.Config, title, textBody string) error {
	addr := net.JoinHostPort(cfg.MailriseHost, strconv.Itoa(cfg.MailrisePort))
	dialer := net.Dialer{Timeout: cfg.NotifyTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

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
	if _, err := w.Write([]byte(buildMIME(cfg, title, textBody))); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close data: %w", err)
	}
	return client.Quit()
}

// buildMIME is N.5.1 step 4: CRLF-terminated headers, then the plain-text
// body (N.3.6) — never payload.Body, which is markdown that renders only
// because the JSON payload carries format: markdown alongside it. Over
// SMTP there is no such field, so mailrise would forward literal
// "**Findings**"/"_Analysis:_" to the operator (verified live 2026-08-18).
func buildMIME(cfg *config.Config, title, textBody string) string {
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
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(textBody)
	return b.String()
}
