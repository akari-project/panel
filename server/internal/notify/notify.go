// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notify 是外发通知（spec/13 13.1）：
//
//   - 业务代码在自己的事务中调用 Enqueue，把消息写入 notification_outbox（OPS-02），不经过事件主题；
//   - 含令牌、验证码或链接的变量加密后存入 secret_variables_enc（CONV-31），投递结束后清除；
//   - worker 的 Deliverer 取出到期消息，按模板渲染后经 Sender 投递，失败时指数退避，超过 retry_until 即最终失败；
//   - 收件地址在投递时从账号读取，队列中不保存邮箱明文（CONV-29）。
//
// 模板只允许白名单变量，不执行任意逻辑（OPS-03）。内置模板见 templates.go；
// notification_templates 中同名、同语言、同渠道的行覆盖内置模板（后台编辑在 M4）。
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// MaxRetry 是一般消息的最长重试时间（OPS-02）。
const MaxRetry = 24 * time.Hour

// secretAD 是 secret_variables_enc 的附加数据。
var secretAD = []byte("notification_outbox.secret_variables_enc")

// Message 是一条待投递的消息。
type Message struct {
	AccountID uuid.UUID
	Template  string
	Locale    string
	// Vars 为普通变量，明文保存。
	Vars map[string]string
	// Secrets 为含令牌、验证码或链接的变量，加密保存（CONV-31）。
	Secrets map[string]string
	// RetryFor 是最长重试时间；验证码与链接类消息等于其有效期（OPS-02）。为 0 时取 MaxRetry。
	RetryFor time.Duration
}

// Outbox 写入通知队列。
type Outbox struct {
	Keys  *secretbox.Keyring
	Clock clock.Clock
}

// Enqueue 在调用方的事务中写入一条邮件消息。模板或变量不在白名单中时返回错误。
func (o Outbox) Enqueue(ctx context.Context, q *sqlc.Queries, m Message) error {
	t, ok := builtin[m.Template]
	if !ok {
		return fmt.Errorf("notify: unknown template %q", m.Template)
	}
	for k := range m.Vars {
		if !t.allowed(k) {
			return fmt.Errorf("notify: %s: variable %q not allowed", m.Template, k)
		}
	}
	for k := range m.Secrets {
		if !t.allowed(k) {
			return fmt.Errorf("notify: %s: variable %q not allowed", m.Template, k)
		}
	}
	vars := m.Vars
	if vars == nil {
		vars = map[string]string{}
	}
	vj, err := json.Marshal(vars)
	if err != nil {
		return err
	}
	var enc []byte
	if len(m.Secrets) > 0 {
		sj, err := json.Marshal(m.Secrets)
		if err != nil {
			return err
		}
		if enc, err = o.Keys.Seal(sj, secretAD); err != nil {
			return err
		}
	}
	retry := m.RetryFor
	if retry <= 0 || retry > MaxRetry {
		retry = MaxRetry
	}
	now := o.Clock.Now()
	id := m.AccountID
	return q.EnqueueNotification(ctx, sqlc.EnqueueNotificationParams{
		AccountID:          &id,
		Channel:            "email",
		Template:           m.Template,
		Locale:             normalizeLocale(m.Locale),
		Variables:          vj,
		SecretVariablesEnc: enc,
		NextAttemptAt:      now,
		RetryUntil:         now.Add(retry),
	})
}
