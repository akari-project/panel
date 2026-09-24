// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// 模板名。安全类消息不可由用户关闭（OPS-04）。
const (
	// TemplateEmailVerification：注册与重新发送的邮箱验证码（AUTH-03）。变量：code（秘密）、minutes。
	TemplateEmailVerification = "email_verification"
	// TemplateRegisterAttempt：有人尝试用已注册的邮箱注册（AUTH-01）。无变量。
	TemplateRegisterAttempt = "register_attempt"
	// TemplatePasswordReset：找回密码链接（AUTH-04）。变量：link（秘密）、minutes。
	TemplatePasswordReset = "password_reset"
	// TemplatePasswordChanged：密码已修改（OPS-04 安全类）。无变量。
	TemplatePasswordChanged = "password_changed"
	// TemplateMFAEnabled、TemplateMFADisabled：二次验证变更（OPS-04 安全类）。无变量。
	TemplateMFAEnabled  = "mfa_enabled"
	TemplateMFADisabled = "mfa_disabled"
	// TemplateRecoveryCodesLow：恢复码剩余不足 3 个（AUTH-11）。变量：remaining。
	TemplateRecoveryCodesLow = "recovery_codes_low"
)

// DefaultLocale 是模板缺少请求的语言时使用的语言。
const DefaultLocale = "zh-CN"

// siteVar 由投递方填入，所有模板都可以使用。
const siteVar = "site_name"

type content struct{ subject, body string }

type template struct {
	vars    []string
	locales map[string]content
}

func (t template) allowed(v string) bool { return v == siteVar || slices.Contains(t.vars, v) }

var builtin = map[string]template{
	TemplateEmailVerification: {
		vars: []string{"code", "minutes"},
		locales: map[string]content{
			"zh-CN": {
				subject: "{{site_name}} 邮箱验证码",
				body:    "你的邮箱验证码是：{{code}}\n\n验证码 {{minutes}} 分钟内有效。如果这不是你本人的操作，请忽略本邮件。\n",
			},
			"en": {
				subject: "{{site_name}} verification code",
				body:    "Your verification code is: {{code}}\n\nThe code expires in {{minutes}} minutes. If you did not request it, you can ignore this email.\n",
			},
		},
	},
	TemplateRegisterAttempt: {
		locales: map[string]content{
			"zh-CN": {
				subject: "{{site_name}}：有人尝试用你的邮箱注册",
				body:    "有人尝试用你的邮箱注册新账号。你已经有账号，无需再次注册；如果忘记了密码，请在登录页选择“找回密码”。\n\n如果这不是你本人的操作，请忽略本邮件，你的账号没有变化。\n",
			},
			"en": {
				subject: "{{site_name}}: someone tried to sign up with your email",
				body:    "Someone tried to create a new account with your email address. You already have an account; if you forgot your password, use \"Forgot password\" on the sign-in page.\n\nIf this wasn't you, you can ignore this email. Your account has not changed.\n",
			},
		},
	},
	TemplatePasswordReset: {
		vars: []string{"link", "minutes"},
		locales: map[string]content{
			"zh-CN": {
				subject: "{{site_name}} 重置密码",
				body:    "请打开以下链接设置新密码：\n\n{{link}}\n\n链接 {{minutes}} 分钟内有效，只能使用一次。如果这不是你本人的操作，请忽略本邮件，你的密码不会改变。\n",
			},
			"en": {
				subject: "Reset your {{site_name}} password",
				body:    "Open the following link to set a new password:\n\n{{link}}\n\nThe link expires in {{minutes}} minutes and can be used once. If you did not request it, you can ignore this email; your password will not change.\n",
			},
		},
	},
	TemplatePasswordChanged: {
		locales: map[string]content{
			"zh-CN": {subject: "{{site_name}}：密码已修改", body: "你的账号密码刚刚被修改，其他设备上的登录已退出。\n\n如果这不是你本人的操作，请立即通过“找回密码”重置密码。\n"},
			"en":    {subject: "{{site_name}}: your password was changed", body: "Your account password was just changed, and other devices were signed out.\n\nIf this wasn't you, reset your password right away using \"Forgot password\".\n"},
		},
	},
	TemplateMFAEnabled: {
		locales: map[string]content{
			"zh-CN": {subject: "{{site_name}}：已启用二次验证", body: "你的账号已启用二次验证（验证器应用）。请妥善保存恢复码。\n\n如果这不是你本人的操作，请立即修改密码。\n"},
			"en":    {subject: "{{site_name}}: two-factor authentication enabled", body: "Two-factor authentication (authenticator app) is now enabled on your account. Keep your recovery codes safe.\n\nIf this wasn't you, change your password right away.\n"},
		},
	},
	TemplateMFADisabled: {
		locales: map[string]content{
			"zh-CN": {subject: "{{site_name}}：已停用二次验证", body: "你的账号已停用二次验证。\n\n如果这不是你本人的操作，请立即修改密码并重新启用二次验证。\n"},
			"en":    {subject: "{{site_name}}: two-factor authentication disabled", body: "Two-factor authentication was disabled on your account.\n\nIf this wasn't you, change your password and enable two-factor authentication again right away.\n"},
		},
	},
	TemplateRecoveryCodesLow: {
		vars: []string{"remaining"},
		locales: map[string]content{
			"zh-CN": {subject: "{{site_name}}：恢复码即将用完", body: "你的二次验证恢复码只剩 {{remaining}} 个。请在“账号安全”中重新生成恢复码。\n"},
			"en":    {subject: "{{site_name}}: recovery codes running low", body: "You have {{remaining}} two-factor recovery codes left. Generate new ones under Account security.\n"},
		},
	},
}

// normalizeLocale 把账号语言映射到模板语言：zh 开头为 zh-CN，en 开头为 en，其余取默认语言。
func normalizeLocale(l string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(l), "zh"):
		return "zh-CN"
	case strings.HasPrefix(strings.ToLower(l), "en"):
		return "en"
	}
	return DefaultLocale
}

var placeholder = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`)

// render 替换 {{变量}}。模板中出现未提供或不在白名单中的变量时返回错误，不输出半成品。
func render(t template, text string, vars map[string]string) (string, error) {
	var missing []string
	out := placeholder.ReplaceAllStringFunc(text, func(m string) string {
		name := placeholder.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok || !t.allowed(name) {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("notify: template variables not available: %s", strings.Join(missing, ", "))
	}
	return out, nil
}
