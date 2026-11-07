package email

import (
	"context"
	"net"
	"net/textproto"
	"strings"
	"testing"
)

func TestSMTPRejectsInsecureRemoteAndHeaderInjection(t *testing.T) {
	if _, err := NewSMTP(Config{Address: "smtp.example.com:25", From: "from@example.test", AllowInsecureLoopback: true}); err == nil {
		t.Fatal("insecure remote SMTP accepted")
	}
	sender, err := NewSMTP(Config{Address: "127.0.0.1:25", From: "from@example.test", AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Send(context.Background(), Message{To: "to@example.test", Subject: "verify\r\nBcc: bad@example.test"}); err == nil {
		t.Fatal("injected headers accepted")
	}
}
func TestSMTPDelivery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		p := textproto.NewConn(conn)
		_ = p.PrintfLine("220 local SMTP")
		for {
			line, e := p.ReadLine()
			if e != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"), strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				_ = p.PrintfLine("250 OK")
			case line == "DATA":
				_ = p.PrintfLine("354 send")
				body, e := p.ReadDotBytes()
				if e != nil {
					return
				}
				received <- string(body)
				_ = p.PrintfLine("250 queued")
			case line == "QUIT":
				_ = p.PrintfLine("221 bye")
				return
			}
		}
	}()
	sender, err := NewSMTP(Config{Address: listener.Addr().String(), From: "from@example.test", AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Send(context.Background(), Message{To: "to@example.test", Subject: "Verify", Body: "single-use link"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(<-received, "single-use link") {
		t.Fatal("body missing")
	}
}
