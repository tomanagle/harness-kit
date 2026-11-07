// Package email supplies an injected mail delivery interface and SMTP adapter.
package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

type Message struct{ To, Subject, Body string }
type Sender interface {
	Send(context.Context, Message) error
}
type Config struct {
	Address, Username, Password, From string
	AllowInsecureLoopback             bool
}
type SMTP struct{ config Config }

func NewSMTP(c Config) (*SMTP, error) {
	host, _, err := net.SplitHostPort(c.Address)
	if err != nil || host == "" {
		return nil, errors.New("email: valid SMTP host:port is required")
	}
	from, err := mail.ParseAddress(c.From)
	if err != nil || from.Address != c.From {
		return nil, errors.New("email: valid SMTP_FROM is required")
	}
	if c.AllowInsecureLoopback && host != "localhost" && !net.ParseIP(host).IsLoopback() {
		return nil, errors.New("email: insecure SMTP is allowed only on loopback")
	}
	return &SMTP{config: c}, nil
}
func MustNewSMTP(c Config) *SMTP {
	sender, err := NewSMTP(c)
	if err != nil {
		panic(err)
	}
	return sender
}
func (s *SMTP) Send(ctx context.Context, m Message) error {
	recipient, err := mail.ParseAddress(m.To)
	if err != nil || recipient.Address != m.To || strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("email: invalid message headers")
	}
	host, _, _ := net.SplitHostPort(s.config.Address)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.config.Address)
	if err != nil {
		return errors.New("email: SMTP connection failed")
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(connection, host)
	if err != nil {
		return errors.New("email: SMTP handshake failed")
	}
	defer client.Close()
	if !s.config.AllowInsecureLoopback {
		if err = client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return errors.New("email: SMTP TLS failed")
		}
	}
	if s.config.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", s.config.Username, s.config.Password, host)); err != nil {
			return errors.New("email: SMTP authentication failed")
		}
	}
	if err = client.Mail(s.config.From); err != nil {
		return errors.New("email: sender rejected")
	}
	if err = client.Rcpt(m.To); err != nil {
		return errors.New("email: recipient rejected")
	}
	data, err := client.Data()
	if err != nil {
		return errors.New("email: SMTP data rejected")
	}
	_, err = fmt.Fprintf(data, "From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", s.config.From, m.To, m.Subject, m.Body)
	if err != nil {
		return errors.New("email: SMTP write failed")
	}
	if err = data.Close(); err != nil {
		return errors.New("email: SMTP delivery failed")
	}
	return client.Quit()
}
