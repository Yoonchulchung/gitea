// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package sender

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"slices"
	"strings"

	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
)

// SMTPSender Sender SMTP mail sender
type SMTPSender struct{}

var _ Sender = &SMTPSender{}

// Send send email
func (s *SMTPSender) Send(from string, to []string, msg io.WriterTo) error {
	opts := setting.MailService

	client, err := dialSMTP()
	if err != nil {
		return err
	}
	defer func() { client.Close() }()

	canAuth, options := client.Extension("AUTH")
	if len(opts.User) > 0 {
		if !canAuth {
			return errors.New("SMTP server does not support AUTH, but credentials provided")
		}

		// Some servers advertise mechanisms they reject, so fall back to the next one like Python's smtplib.
		// A failed client.Auth quits the session, so each retry needs a new connection.
		auths := smtpAuths(options, opts.User, opts.Passwd)
		for i, auth := range auths {
			if err = client.Auth(auth); err == nil {
				break
			}
			if i == len(auths)-1 {
				return fmt.Errorf("failed to authenticate SMTP: %w", err)
			}
			log.Warn("SMTP authentication failed, trying the next mechanism: %v", err)
			client.Close()
			next, err := dialSMTP()
			if err != nil {
				return err
			}
			client = next
		}
	}

	fromAddr := from
	if opts.OverrideEnvelopeFrom && opts.EnvelopeFrom != "" {
		fromAddr = opts.EnvelopeFrom
	}
	smtpFrom, err := sanitizeEmailAddress(fromAddr)
	if err != nil {
		return fmt.Errorf("invalid envelope from address: %w", err)
	}
	if err = client.Mail(smtpFrom); err != nil {
		return fmt.Errorf("failed to issue MAIL command: %w", err)
	}

	for _, rec := range to {
		smtpTo, err := sanitizeEmailAddress(rec)
		if err != nil {
			return fmt.Errorf("invalid recipient address %q: %w", rec, err)
		}
		if err = client.Rcpt(smtpTo); err != nil {
			return fmt.Errorf("failed to issue RCPT command: %w", err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("failed to issue DATA command: %w", err)
	} else if _, err = msg.WriteTo(w); err != nil {
		return fmt.Errorf("SMTP write failed: %w", err)
	} else if err = w.Close(); err != nil {
		return fmt.Errorf("SMTP close failed: %w", err)
	}

	err = client.Quit()
	if err != nil {
		log.Error("Quit client failed: %v", err)
	}

	return nil
}

func dialSMTP() (*smtp.Client, error) {
	opts := setting.MailService

	var network string
	var address string
	if opts.Protocol == "smtp+unix" {
		network = "unix"
		address = opts.SMTPAddr
	} else {
		network = "tcp"
		address = net.JoinHostPort(opts.SMTPAddr, opts.SMTPPort)
	}

	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to establish network connection to SMTP server: %w", err)
	}

	client, err := initSMTPClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

func initSMTPClient(conn net.Conn) (*smtp.Client, error) {
	opts := setting.MailService

	var tlsconfig *tls.Config
	if opts.Protocol == "smtps" || opts.Protocol == "smtp+starttls" {
		tlsconfig = &tls.Config{
			InsecureSkipVerify: opts.ForceTrustServerCert,
			ServerName:         opts.SMTPAddr,
		}

		if opts.UseClientCert {
			cert, err := tls.LoadX509KeyPair(opts.ClientCertFile, opts.ClientKeyFile)
			if err != nil {
				return nil, fmt.Errorf("could not load SMTP client certificate: %w", err)
			}
			tlsconfig.Certificates = []tls.Certificate{cert}
		}
	}

	if opts.Protocol == "smtps" {
		conn = tls.Client(conn, tlsconfig)
	}

	client, err := smtp.NewClient(conn, smtpServerName())
	if err != nil {
		return nil, fmt.Errorf("could not initiate SMTP session: %w", err)
	}

	if opts.EnableHelo {
		hostname := opts.HeloHostname
		if len(hostname) == 0 {
			hostname, err = os.Hostname()
			if err != nil {
				return nil, fmt.Errorf("could not retrieve system hostname: %w", err)
			}
		}

		if err = client.Hello(hostname); err != nil {
			return nil, fmt.Errorf("failed to issue HELO command: %w", err)
		}
	}

	if opts.Protocol == "smtp+starttls" {
		hasStartTLS, _ := client.Extension("STARTTLS")
		if hasStartTLS {
			if err = client.StartTLS(tlsconfig); err != nil {
				return nil, fmt.Errorf("failed to start TLS connection: %w", err)
			}
		} else {
			log.Warn("StartTLS requested, but SMTP server does not support it; falling back to regular SMTP")
		}
	}
	return client, nil
}

func smtpServerName() string {
	if setting.MailService.Protocol == "smtp+unix" {
		return setting.MailService.SMTPAddr
	}
	return "localhost"
}

// smtpAuths returns the supported mechanisms the server advertises, in order of preference
func smtpAuths(options, user, passwd string) []smtp.Auth {
	advertised := strings.Fields(strings.ToUpper(options))
	var auths []smtp.Auth
	if slices.Contains(advertised, "CRAM-MD5") {
		auths = append(auths, smtp.CRAMMD5Auth(user, passwd))
	}
	if slices.Contains(advertised, "PLAIN") {
		auths = append(auths, smtp.PlainAuth("", user, passwd, smtpServerName()))
	}
	if slices.Contains(advertised, "LOGIN") {
		auths = append(auths, LoginAuth(user, passwd))
	}
	if slices.Contains(advertised, "NTLM") {
		auths = append(auths, NtlmAuth(user, passwd))
	}
	return auths
}

func sanitizeEmailAddress(raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(strings.Trim(raw, "<>")))
	if err != nil {
		return "", err
	}
	return addr.Address, nil
}
