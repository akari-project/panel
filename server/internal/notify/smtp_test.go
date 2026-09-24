// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strconv"
	"strings"
	"testing"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/testdb"
)

// fakeSMTP 是最小的 SMTP 服务器：不支持 STARTTLS 与 AUTH，收到的邮件写入 got。
// rcptCode 非 0 时对 RCPT 返回该回复码；implicit 非 nil 时在该 TLS 配置下监听（隐式 TLS）。
func fakeSMTP(t *testing.T, rcptCode int, implicit *tls.Config) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit != nil {
		ln = tls.NewListener(ln, implicit)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
		say("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "MAIL FROM"):
				say("250 ok")
			case strings.HasPrefix(cmd, "RCPT TO"):
				if rcptCode != 0 {
					say(strconv.Itoa(rcptCode) + " no such user nobody@example.com")
					continue
				}
				say("250 ok")
			case cmd == "DATA":
				say("354 go ahead")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				got <- b.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

// sender 写入 smtp 设置；mode 为空时不写 tls 键（取缺省 starttls）。
func sender(t *testing.T, addr, username, mode string) SMTPSender {
	t.Helper()
	pool := testdb.New(t)
	host, port, _ := net.SplitHostPort(addr)
	cfg := `{"host":"` + host + `","port":` + port + `,"username":"` + username + `","from_address":"Akari <noreply@example.com>"`
	if mode != "" {
		cfg += `,"tls":"` + mode + `"`
	}
	cfg += `}`
	if _, err := pool.Exec(context.Background(), `INSERT INTO settings (key, value) VALUES ('smtp', $1)`, cfg); err != nil {
		t.Fatal(err)
	}
	return SMTPSender{Pool: pool, Keys: testKeys(t), Clock: clock.NewFake(t0)}
}

func TestSMTPSend(t *testing.T) {
	addr, got := fakeSMTP(t, 0, nil)
	s := sender(t, addr, "", TLSNone)
	err := s.Send(context.Background(), Email{To: "u@example.com", Subject: "Akari 邮箱验证码", Body: "验证码：123456\n第二行\n"})
	if err != nil {
		t.Fatal(err)
	}
	raw := <-got
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if subject != "Akari 邮箱验证码" || msg.Header.Get("To") != "<u@example.com>" ||
		!strings.Contains(msg.Header.Get("From"), "noreply@example.com") || msg.Header.Get("Date") == "" {
		t.Fatalf("headers = %v", msg.Header)
	}
	if string(body) != "验证码：123456\r\n第二行\r\n" {
		t.Fatalf("body = %q", body)
	}
}

// 回复码归类；错误文本不含服务器返回的内容（其中可能有收件地址，CONV-24）。
func TestSMTPErrors(t *testing.T) {
	addr, _ := fakeSMTP(t, 550, nil)
	s := sender(t, addr, "", TLSNone)
	err := s.Send(context.Background(), Email{To: "nobody@example.com", Subject: "s", Body: "b"})
	var se *SMTPError
	if !errors.As(err, &se) || se.Class() != "smtp_rcpt_5xx" || strings.Contains(err.Error(), "@") {
		t.Fatalf("err = %v", err)
	}

}

// 加密方式（spec/03 3.6）：缺省要求 STARTTLS，不降级；none 不允许认证；implicit 校验服务器证书。
func TestSMTPTLSModes(t *testing.T) {
	m := Email{To: "u@example.com", Subject: "s", Body: "b"}
	var se *SMTPError
	for _, tc := range []struct{ name, username, mode string }{
		{"starttls default, server lacks STARTTLS", "", ""},
		{"explicit starttls, server lacks STARTTLS", "user", TLSStartTLS},
		{"none with username", "user", TLSNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, got := fakeSMTP(t, 0, nil)
			if err := sender(t, addr, tc.username, tc.mode).Send(context.Background(), m); !errors.As(err, &se) || se.Class() != "smtp_tls" {
				t.Fatalf("err = %v", err)
			}
			select {
			case <-got:
				t.Fatal("message delivered")
			default:
			}
		})
	}

	t.Run("unknown mode", func(t *testing.T) {
		addr, _ := fakeSMTP(t, 0, nil)
		if err := sender(t, addr, "", "ssl").Send(context.Background(), m); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("err = %v", err)
		}
	})

	// httptest 的测试证书包含 127.0.0.1，用作隐式 TLS 服务器的证书。
	ts := httptest.NewUnstartedServer(nil)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	roots := ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	serverTLS := &tls.Config{Certificates: ts.TLS.Certificates}

	t.Run("implicit", func(t *testing.T) {
		addr, got := fakeSMTP(t, 0, serverTLS)
		s := sender(t, addr, "", TLSImplicit)
		s.RootCAs = roots
		if err := s.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		if raw := <-got; !strings.Contains(raw, "Subject: s") {
			t.Fatalf("message = %q", raw)
		}
	})

	t.Run("implicit untrusted certificate", func(t *testing.T) {
		addr, got := fakeSMTP(t, 0, serverTLS)
		s := sender(t, addr, "", TLSImplicit)
		s.RootCAs = x509.NewCertPool()
		if err := s.Send(context.Background(), m); !errors.As(err, &se) || se.Class() != "smtp_tls" {
			t.Fatalf("err = %v", err)
		}
		select {
		case <-got:
			t.Fatal("message delivered")
		default:
		}
	})
}

func TestSMTPNotConfigured(t *testing.T) {
	s := SMTPSender{Pool: testdb.New(t), Keys: testKeys(t), Clock: clock.NewFake(t0)}
	if err := s.Send(context.Background(), Email{To: "u@example.com"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestSMTPPasswordDecrypted(t *testing.T) {
	s := sender(t, "127.0.0.1:1", "user", "")
	ct, _ := s.Keys.Seal([]byte("s3cret"), passwordAD)
	v := `"` + base64.StdEncoding.EncodeToString(ct) + `"`
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO settings (key, value) VALUES ('smtp_password_enc', $1)`, v); err != nil {
		t.Fatal(err)
	}
	c, pw, err := s.config(context.Background())
	if err != nil || pw != "s3cret" || c.Username != "user" {
		t.Fatalf("config = %+v %q %v", c, pw, err)
	}
}
