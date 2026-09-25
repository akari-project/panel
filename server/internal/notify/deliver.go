// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// Email 是一封待发送的邮件。
type Email struct {
	To, Subject, Body string
}

// Sender 投递邮件（OPS-01 的 Notifier 的邮件渠道）。
type Sender interface {
	Send(ctx context.Context, m Email) error
}

// ErrNotConfigured 表示渠道尚未配置（例如后台还没有填写 SMTP），按普通失败退避重试。
var ErrNotConfigured = errors.New("notify: channel not configured")

// Deliverer 投递通知队列中到期的消息。
type Deliverer struct {
	Pool     *pgxpool.Pool
	Keys     *secretbox.Keyring
	Clock    clock.Clock
	Sender   Sender
	Log      *slog.Logger
	SiteName string
	// Batch 是每次最多处理的条数，默认 20。
	Batch int
}

// 退避：第 n 次失败后等待 min(30 秒 × 2^(n-1), 1 小时)。
func backoff(attempts int32) time.Duration {
	d := 30 * time.Second
	for i := int32(1); i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// RunOnce 投递至多 Batch 条到期消息，返回已处理（成功或失败）的条数。
// 每条消息在自己的事务中锁定、投递并记录结果：行锁只在投递这一条时持有，
// 某条的数据库错误不会让已发出的其他邮件回滚后重发。多个 worker 并行时互不重复（SKIP LOCKED）。
func (d *Deliverer) RunOnce(ctx context.Context) (int, error) {
	batch := d.Batch
	if batch <= 0 {
		batch = 20
	}
	n := 0
	for range batch {
		found := false
		err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
			q := sqlc.New(tx)
			rows, err := q.ClaimDueNotifications(ctx, sqlc.ClaimDueNotificationsParams{Now: d.Clock.Now(), MaxRows: 1})
			if err != nil || len(rows) == 0 {
				return err
			}
			found = true
			return d.deliver(ctx, q, rows[0])
		})
		if err != nil {
			return n, err
		}
		if !found {
			break
		}
		n++
	}
	return n, nil
}

// deliver 投递一条消息并记录结果。只有数据库错误返回 error；投递失败记录在行中。
func (d *Deliverer) deliver(ctx context.Context, q *sqlc.Queries, row sqlc.ClaimDueNotificationsRow) error {
	now := d.Clock.Now()
	if row.InvitationClosed {
		// 邀请已被接受、撤销或已过期：不再投递，按最终失败处理并清除秘密变量（AUTH-22、CONV-31）。
		class := "invitation_closed"
		return q.MarkNotificationFailed(ctx, sqlc.MarkNotificationFailedParams{ID: row.ID, Now: &now, LastError: &class})
	}
	sendErr := d.send(ctx, q, row)
	if sendErr == nil {
		return q.MarkNotificationSent(ctx, sqlc.MarkNotificationSentParams{ID: row.ID, Now: &now})
	}
	// last_error 与日志只记录错误类别：SMTP 错误文本可能含收件地址（CONV-24）。
	class := errorClass(sendErr)
	next := now.Add(backoff(row.Attempts + 1))
	if !next.Before(row.RetryUntil) {
		d.logFailure(ctx, row.ID, row.Template, class, true)
		return q.MarkNotificationFailed(ctx, sqlc.MarkNotificationFailedParams{ID: row.ID, Now: &now, LastError: &class})
	}
	d.logFailure(ctx, row.ID, row.Template, class, false)
	return q.MarkNotificationRetry(ctx, sqlc.MarkNotificationRetryParams{ID: row.ID, NextAttemptAt: next, LastError: &class})
}

func (d *Deliverer) logFailure(ctx context.Context, id uuid.UUID, template, class string, final bool) {
	if d.Log == nil {
		return
	}
	d.Log.LogAttrs(ctx, slog.LevelWarn, "notification delivery failed",
		slog.String("notification_id", id.String()), slog.String("template", template),
		slog.String("error", class), slog.Bool("final", final))
}

func (d *Deliverer) send(ctx context.Context, q *sqlc.Queries, row sqlc.ClaimDueNotificationsRow) error {
	if row.Channel != "email" {
		return errUnsupportedChannel
	}
	t, ok := builtin[row.Template]
	if !ok {
		return errUnknownTemplate
	}
	vars := map[string]string{}
	if err := json.Unmarshal(row.Variables, &vars); err != nil {
		return errBadVariables
	}
	if len(row.SecretVariablesEnc) > 0 {
		pt, err := d.Keys.Open(row.SecretVariablesEnc, secretAD)
		if err != nil {
			return errBadVariables
		}
		secrets := map[string]string{}
		if err := json.Unmarshal(pt, &secrets); err != nil {
			return errBadVariables
		}
		for k, v := range secrets {
			vars[k] = v
		}
	}
	vars[siteVar] = d.SiteName

	c, err := d.content(ctx, q, t, row.Template, row.Locale)
	if err != nil {
		return err
	}
	subject, err := render(t, c.subject, vars)
	if err != nil {
		return errBadTemplate
	}
	body, err := render(t, c.body, vars)
	if err != nil {
		return errBadTemplate
	}
	return d.Sender.Send(ctx, Email{To: row.Email, Subject: subject, Body: body})
}

// content 优先取 notification_templates 中的覆盖，其次取内置模板；缺少该语言时取默认语言。
func (d *Deliverer) content(ctx context.Context, q *sqlc.Queries, t template, name, locale string) (content, error) {
	for _, l := range []string{locale, DefaultLocale} {
		row, err := q.GetNotificationTemplate(ctx, sqlc.GetNotificationTemplateParams{Template: name, Locale: l, Channel: "email"})
		if err == nil {
			c := content{body: row.Body}
			if row.Subject != nil {
				c.subject = *row.Subject
			}
			return c, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return content{}, err
		}
		if c, ok := t.locales[l]; ok {
			return c, nil
		}
	}
	return content{}, errUnknownTemplate
}

var (
	errUnsupportedChannel = errors.New("unsupported_channel")
	errUnknownTemplate    = errors.New("unknown_template")
	errBadVariables       = errors.New("bad_variables")
	errBadTemplate        = errors.New("bad_template")
)

// errorClass 把错误归为不含个人信息的类别。
func errorClass(err error) string {
	var se *SMTPError
	switch {
	case errors.Is(err, ErrNotConfigured):
		return "not_configured"
	case errors.As(err, &se):
		return se.Class()
	case errors.Is(err, errUnsupportedChannel), errors.Is(err, errUnknownTemplate),
		errors.Is(err, errBadVariables), errors.Is(err, errBadTemplate):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "error"
}
