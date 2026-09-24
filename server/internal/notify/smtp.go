// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// SMTPConfig 是设置项 smtp 的取值（字段与管理接口的 settings.smtp 同名，spec/03 3.6）。
// 密码保存在设置项 smtp_password_enc（CONV-19），不在此对象中。
type SMTPConfig struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	FromAddress string `json:"from_address"`
	// TLS 是连接加密方式（spec/03 3.6）：starttls（缺省，要求 STARTTLS）、implicit（隐式 TLS，常用端口 465）、
	// none（明文，只用于本机或可信内网中继）。不提供跳过证书校验的选项。
	TLS string `json:"tls"`
}

// 加密方式。
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "implicit"
	TLSNone     = "none"
)

// SMTPError 是 SMTP 投递错误。Error 不含服务器返回的文本（可能含收件地址，CONV-24）。
type SMTPError struct {
	Stage string // connect、tls、auth、mail、rcpt、data
	Code  int    // SMTP 回复码；连接类错误为 0
}

func (e *SMTPError) Error() string { return e.Class() }

// Class 返回错误类别，例如 smtp_rcpt_5xx、smtp_connect。
func (e *SMTPError) Class() string {
	if e.Code == 0 {
		return "smtp_" + e.Stage
	}
	return fmt.Sprintf("smtp_%s_%dxx", e.Stage, e.Code/100)
}

// SMTPSender 按设置项 smtp 投递邮件，每次发送时读取设置，后台修改后立即生效。
type SMTPSender struct {
	Pool  *pgxpool.Pool
	Keys  *secretbox.Keyring
	Clock clock.Clock
	// Timeout 是单封邮件的时限，默认 30 秒。
	Timeout time.Duration
	// RootCAs 为校验服务器证书的根证书，nil 时使用系统根证书（测试中替换）。
	RootCAs *x509.CertPool
}

var passwordAD = []byte("settings.smtp_password_enc")

func (s SMTPSender) config(ctx context.Context) (SMTPConfig, string, error) {
	q := sqlc.New(s.Pool)
	raw, err := q.GetSetting(ctx, "smtp")
	if errors.Is(err, pgx.ErrNoRows) {
		return SMTPConfig{}, "", ErrNotConfigured
	}
	if err != nil {
		return SMTPConfig{}, "", err
	}
	var c SMTPConfig
	if err := json.Unmarshal(raw, &c); err != nil || c.Host == "" || c.FromAddress == "" {
		return SMTPConfig{}, "", ErrNotConfigured
	}
	if c.Port == 0 {
		c.Port = 587
	}
	var password string
	enc, err := q.GetSetting(ctx, "smtp_password_enc")
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return SMTPConfig{}, "", err
	default:
		var b64 string
		if err := json.Unmarshal(enc, &b64); err != nil {
			return SMTPConfig{}, "", ErrNotConfigured
		}
		ct, err := decodeBase64(b64)
		if err != nil {
			return SMTPConfig{}, "", ErrNotConfigured
		}
		pt, err := s.Keys.Open(ct, passwordAD)
		if err != nil {
			return SMTPConfig{}, "", ErrNotConfigured
		}
		password = string(pt)
	}
	return c, password, nil
}

// Send 投递一封纯文本邮件。加密方式按配置的 tls 取值（缺省 starttls，服务器不支持时失败，不降级）；
// 配置了用户名时进行认证，tls 为 none 时拒绝发送，不在明文连接上传输密码。
func (s SMTPSender) Send(ctx context.Context, m Email) error {
	c, password, err := s.config(ctx)
	if err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	from, err := mail.ParseAddress(c.FromAddress)
	if err != nil {
		return ErrNotConfigured
	}
	mode := c.TLS
	if mode == "" {
		mode = TLSStartTLS
	}
	if mode != TLSStartTLS && mode != TLSImplicit && mode != TLSNone {
		return ErrNotConfigured
	}
	if mode == TLSNone && c.Username != "" {
		// 不在明文连接上发送密码（CONV-19），保存设置时同样拒绝该组合。
		return &SMTPError{Stage: "tls"}
	}
	tlsConf := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12, RootCAs: s.RootCAs}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	var conn net.Conn
	if mode == TLSImplicit {
		conn, err = (&tls.Dialer{Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		var cv *tls.CertificateVerificationError
		if errors.As(err, &cv) {
			return &SMTPError{Stage: "tls"}
		}
		return &SMTPError{Stage: "connect"}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return smtpErr("connect", err)
	}
	defer cl.Close()
	if mode == TLSStartTLS {
		// 要求加密：服务器不提供 STARTTLS（或被中间人剥离）时失败，不降级为明文（spec/03 3.6）。
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return &SMTPError{Stage: "tls"}
		}
		if err := cl.StartTLS(tlsConf); err != nil {
			return smtpErr("tls", err)
		}
	}
	if c.Username != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.Username, password, c.Host)); err != nil {
			return smtpErr("auth", err)
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return smtpErr("mail", err)
	}
	if err := cl.Rcpt(m.To); err != nil {
		return smtpErr("rcpt", err)
	}
	w, err := cl.Data()
	if err != nil {
		return smtpErr("data", err)
	}
	if _, err := w.Write(s.message(from, m)); err != nil {
		return smtpErr("data", err)
	}
	if err := w.Close(); err != nil {
		return smtpErr("data", err)
	}
	_ = cl.Quit()
	return nil
}

func smtpErr(stage string, err error) *SMTPError {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return &SMTPError{Stage: stage, Code: tp.Code}
	}
	return &SMTPError{Stage: stage}
}

// message 生成 RFC 5322 邮件：UTF-8 纯文本，quoted-printable 编码，主题按 RFC 2047 编码。
func (s SMTPSender) message(from *mail.Address, m Email) []byte {
	var b bytes.Buffer
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", from.String())
	hdr("To", (&mail.Address{Address: m.To}).String())
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", s.Clock.Now().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	hdr("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n")))
	_ = qp.Close()
	return b.Bytes()
}

func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
