// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	system_model "gitea.dev/models/system"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
)

// Mail the platform sends about itself.
//
// Gitea has a mailer of its own, configured in app.ini and used for its own
// notifications. This is deliberately not that one. What an administrator
// needs here is to point the platform's own messages at a server they
// control and change who receives them without an operator, a file edit and
// a restart — which is the same reason the support address and the AI
// provider live in the database (company/adminsettings.go).
//
// The password is encrypted at rest with the instance's SECRET_KEY, the way
// every other secret this platform holds is (company/secretstore.go). That
// protects a database dump, not the administrator who typed it.

const (
	settingKeyMailServer   = "company.mail.server"
	settingKeyMailPassword = "company.mail.password"
)

// Transport security, as the form offers it.
const (
	mailSecurityNone     = "none"
	mailSecurityStartTLS = "starttls"
	mailSecurityTLS      = "tls"
)

// MailServer is where the platform's mail goes out through.
type MailServer struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// From is the envelope and header sender; FromName is what a reader sees
	// instead of the address.
	From     string `json:"from"`
	FromName string `json:"fromName"`
	Security string `json:"security"`
	// SkipVerify accepts a certificate that does not check out. Internal mail
	// servers often have one; saying so explicitly is better than a platform
	// that quietly does not verify anything.
	SkipVerify bool `json:"skipVerify"`
}

// Sender is the address mail goes out as: From when it is set, otherwise the
// login, which on most servers is the mailbox itself.
func (m MailServer) Sender() string {
	if m.From != "" {
		return m.From
	}
	if addr, err := mail.ParseAddress(m.Username); err == nil {
		return addr.Address
	}
	return ""
}

// Configured reports whether there is enough here to send anything.
func (m MailServer) Configured() bool { return m.Host != "" && m.Port > 0 && m.Sender() != "" }

// Addr is the host:port to dial.
func (m MailServer) Addr() string { return net.JoinHostPort(m.Host, strconv.Itoa(m.Port)) }

// mailCache holds the server settings, which are read whenever something is
// sent and written only from the one form that owns them.
var (
	mailMu     sync.RWMutex
	mailLoaded bool
	mailServer MailServer
	mailPass   string
)

func loadMailSettings(ctx context.Context) {
	mailMu.RLock()
	loaded := mailLoaded
	mailMu.RUnlock()
	if loaded {
		return
	}
	// Read before the lock, like the platform settings next door: holding a
	// write lock across a database round-trip serialises every caller behind
	// it, and a duplicated read during a cold start costs nothing.
	_, all, err := system_model.GetAllSettings(ctx)
	if err != nil {
		log.Error("company: reading the mail settings: %v", err)
		return
	}
	var server MailServer
	if raw := all[settingKeyMailServer]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &server); err != nil {
			log.Error("company: the stored mail settings are unreadable: %v", err)
		}
	}
	pass := ""
	if stored := all[settingKeyMailPassword]; stored != "" {
		if decrypted, err := secret_module.DecryptSecret(setting.SecretKey, stored); err == nil {
			pass = decrypted
		} else {
			log.Error("company: the stored mail password could not be decrypted: %v", err)
		}
	}

	mailMu.Lock()
	defer mailMu.Unlock()
	mailServer, mailPass, mailLoaded = server, pass, true
}

// MailSettings is the server as the form shows it. The password is never
// returned — a form that renders it puts it in every page cache and browser
// history between here and the administrator.
func MailSettings(ctx context.Context) MailServer {
	loadMailSettings(ctx)
	mailMu.RLock()
	defer mailMu.RUnlock()
	return mailServer
}

// MailPasswordSet reports whether one is stored, which is all a form needs
// to say "leave blank to keep it".
func MailPasswordSet(ctx context.Context) bool {
	loadMailSettings(ctx)
	mailMu.RLock()
	defer mailMu.RUnlock()
	return mailPass != ""
}

// SaveMailSettings stores the server. An empty password keeps the one
// already there, so re-saving the host does not force the administrator to
// retype a secret they cannot read back.
func SaveMailSettings(ctx context.Context, server MailServer, password string) error {
	raw, err := json.Marshal(server)
	if err != nil {
		return err
	}
	values := map[string]string{settingKeyMailServer: string(raw)}
	if password != "" {
		encrypted, err := secret_module.EncryptSecret(setting.SecretKey, password)
		if err != nil {
			return err
		}
		values[settingKeyMailPassword] = encrypted
	}
	if err := system_model.SetSettings(ctx, values); err != nil {
		return err
	}
	invalidateMailSettings()
	return nil
}

// ClearMailPassword removes the stored one — the explicit action that an
// empty field deliberately is not.
func ClearMailPassword(ctx context.Context) error {
	if err := system_model.SetSettings(ctx, map[string]string{settingKeyMailPassword: ""}); err != nil {
		return err
	}
	invalidateMailSettings()
	return nil
}

func invalidateMailSettings() {
	mailMu.Lock()
	mailLoaded = false
	mailMu.Unlock()
}

// mailTimeout bounds a send. A mail server that hangs must not hang the
// request that triggered it, nor the deploy behind that.
const mailTimeout = 20 * time.Second

// SendPlatformMail sends one HTML message to everyone in to.
//
// One message with every recipient on it rather than one each: these go to a
// handful of internal addresses that already know about each other, and a
// single send is a single thing to retry, log and fail.
func SendPlatformMail(ctx context.Context, to []string, subject, htmlBody string) error {
	loadMailSettings(ctx)
	mailMu.RLock()
	server, pass := mailServer, mailPass
	mailMu.RUnlock()

	if !server.Configured() {
		return userKeyError("company.mail.err_unconfigured")
	}
	to = cleanAddresses(to)
	if len(to) == 0 {
		return userKeyError("company.mail.err_no_recipients")
	}
	return sendSMTP(ctx, server, pass, to, buildMessage(server, to, subject, htmlBody))
}

// buildMessage assembles the wire format: an HTML body, declared as such,
// with the headers a mail server expects to see.
func buildMessage(server MailServer, to []string, subject, htmlBody string) []byte {
	from := server.Sender()
	if server.FromName != "" {
		from = mime.QEncoding.Encode("utf-8", server.FromName) + " <" + from + ">"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	return []byte(b.String())
}

// sendSMTP dials, secures, authenticates and sends.
//
// net/smtp rather than a mail library: Gitea's own sender is built on it
// (services/mailer/sender/smtp.go), so this adds no dependency and behaves
// the same way against the same servers.
func sendSMTP(ctx context.Context, server MailServer, password string, to []string, msg []byte) error {
	dialer := &net.Dialer{Timeout: mailTimeout}
	tlsConfig := &tls.Config{ServerName: server.Host, InsecureSkipVerify: server.SkipVerify} //nolint:gosec // the administrator asked for it, explicitly, on the form

	var conn net.Conn
	var err error
	if server.Security == mailSecurityTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", server.Addr(), tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", server.Addr())
	}
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", server.Addr(), err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(mailTimeout))

	client, err := smtp.NewClient(conn, server.Host)
	if err != nil {
		return err
	}
	defer client.Close()

	if server.Security == mailSecurityStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if server.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", server.Username, password, server.Host)); err != nil {
			return fmt.Errorf("signing in as %s: %w", server.Username, err)
		}
	}
	if err := client.Mail(server.Sender()); err != nil {
		return err
	}
	for _, addr := range to {
		if err := client.Rcpt(addr); err != nil {
			return fmt.Errorf("the server refused %s: %w", addr, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// cleanAddresses drops blanks and duplicates from a list somebody typed.
func cleanAddresses(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, raw := range list {
		addr := strings.TrimSpace(raw)
		if addr == "" || seen[strings.ToLower(addr)] {
			continue
		}
		seen[strings.ToLower(addr)] = true
		out = append(out, addr)
	}
	return out
}

// SplitAddresses reads a textarea of recipients — one per line, or separated
// by commas or semicolons, because people paste all three.
func SplitAddresses(raw string) []string {
	return cleanAddresses(strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	}))
}
