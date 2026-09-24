// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth/token"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// Device 是登录请求中的设备信息（spec/30 DeviceInfo）。
type Device struct {
	Platform   string
	Model      *string
	AppVersion *string
	// PublicKey 为 Ed25519 公钥的 SPKI DER base64；非 web 设备必填。
	PublicKey *string
	// ReuseID 与 Proof 一起提交时复用原设备记录与名额。
	ReuseID *uuid.UUID
	Proof   *Proof
}

// Proof 是设备证明：设备私钥对 nonce 字符串 UTF-8 字节的签名（base64）。
type Proof struct{ Nonce, Signature string }

// Login 是密码登录请求。
type Login struct {
	Email, Password string
	Device          Device
	IP              string // 限流主体
	IPPrefix        string // /24 或 /48（CONV-24）
	UserAgent       string
}

// Result 是登录结果。
type Result struct {
	Tokens
	CredentialStatus string // issued、device_limit_reached、entitlement_inactive、web_device
	IsWeb            bool
}

var platforms = []string{"ios", "android", "windows", "macos", "linux", "web", "other"}

// PasswordLogin 执行第一步登录（AUTH-09、AUTH-10、AUTH-20）。
// 邮箱不存在与密码错误返回相同的 401，并都恰好执行一次 argon2id；启用二次验证的账号返回 mfa_required。
func (s *Service) PasswordLogin(ctx context.Context, in Login) (Result, error) {
	email, err := account.NormalizeEmail(in.Email)
	if err != nil {
		return Result{}, err
	}
	if in.Password == "" || len(in.Password) > 4*128 {
		return Result{}, apierr.Invalid(apierr.Field("password", "invalid_format"))
	}
	pubKey, err := s.validateDevice(in.Device)
	if err != nil {
		return Result{}, err
	}
	if err := s.reserveAttempt(ctx, in.IP, email); err != nil {
		return Result{}, err
	}

	q := sqlc.New(s.Pool)
	acct, err := q.LoginAccount(ctx, email)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	hash := DummyHash
	if found && acct.PasswordHash != nil {
		hash = *acct.PasswordHash
	}
	ok, err := s.verify(in.Password, hash)
	if err != nil {
		return Result{}, err
	}
	if !found || acct.PasswordHash == nil || !ok {
		return Result{}, apierr.Unauthenticated // 预占的尝试不释放，即计为一次失败
	}
	if acct.Status != "active" {
		return Result{}, apierr.New(403, "account_suspended")
	}
	if acct.TotpEnabled {
		// 不清除失败计数：二次验证完成后才算登录成功。
		return Result{}, s.startMFA(ctx, acct.ID, in)
	}
	if err := s.clearAttempts(ctx, email); err != nil {
		return Result{}, err
	}
	return s.complete(ctx, acct.ID, in, pubKey, []string{"pwd"})
}

// validateDevice 校验设备信息，返回非 web 设备的公钥。
func (s *Service) validateDevice(d Device) (ed25519.PublicKey, error) {
	if !contains(platforms, d.Platform) {
		return nil, apierr.Invalid(apierr.Field("device.platform", "invalid_format"))
	}
	if d.Model != nil && len(*d.Model) > 100 {
		return nil, apierr.Invalid(apierr.Field("device.model", "too_long"))
	}
	if d.AppVersion != nil && len(*d.AppVersion) > 32 {
		return nil, apierr.Invalid(apierr.Field("device.app_version", "too_long"))
	}
	if d.Platform == "web" {
		return nil, nil
	}
	if d.PublicKey == nil {
		return nil, apierr.Invalid(apierr.Field("device.public_key", "required"))
	}
	pk, err := parsePublicKey(*d.PublicKey)
	if err != nil {
		return nil, apierr.Invalid(apierr.Field("device.public_key", "invalid_format"))
	}
	return pk, nil
}

func parsePublicKey(b64 string) (ed25519.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pk, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return pk, nil
}

func normalize(email string) (string, error) { return account.NormalizeEmail(email) }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// complete 注册设备、决定凭据状态并建立会话（AUTH-10、AUTH-14）。
func (s *Service) complete(ctx context.Context, acct uuid.UUID, in Login, pubKey ed25519.PublicKey, amr []string) (Result, error) {
	var res Result
	res.IsWeb = in.Device.Platform == "web"
	var revoked []uuid.UUID
	// 设备复用的 nonce 在事务外消费（Valkey），只能使用一次。
	reuse := uuid.Nil
	if !res.IsWeb && in.Device.ReuseID != nil && in.Device.Proof != nil && s.consumeNonce(ctx, in.Device.Proof.Nonce) {
		reuse = *in.Device.ReuseID
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		now := s.Clock.Now()
		device, err := s.registerDevice(ctx, q, acct, in, pubKey, reuse)
		if err != nil {
			return err
		}
		if res.IsWeb {
			res.CredentialStatus = "web_device"
			// 超出 50 个的 web 设备连同其会话吊销（AUTH-10）。
			over, err := q.ActiveWebDevicesOverLimit(ctx, sqlc.ActiveWebDevicesOverLimitParams{AccountID: acct, Keep: MaxWebDevices})
			if err != nil {
				return err
			}
			for _, d := range over {
				if err := q.RevokeDevice(ctx, sqlc.RevokeDeviceParams{ID: d, Now: &now}); err != nil {
					return err
				}
				ids, err := q.RevokeDeviceSessions(ctx, sqlc.RevokeDeviceSessionsParams{DeviceID: &d, Now: &now})
				if err != nil {
					return err
				}
				revoked = append(revoked, ids...)
			}
		} else if res.CredentialStatus, err = s.issueCredential(ctx, q, acct, device); err != nil {
			return err
		}
		res.Tokens, err = s.newSession(ctx, q, acct, device, token.AudienceClient, nil, in.UserAgent, in.IPPrefix,
			ClientIdle, now.Add(ClientAbsolute), amr)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	if len(revoked) > 0 {
		if err := s.revokeAfterCommit(ctx, revoked); err != nil {
			return Result{}, apierr.Unavailable(err)
		}
	}
	return res, nil
}

// registerDevice 复用经过证明的设备，否则注册新设备（AUTH-10）。
func (s *Service) registerDevice(ctx context.Context, q *sqlc.Queries, acct uuid.UUID, in Login, pubKey ed25519.PublicKey, reuse uuid.UUID) (uuid.UUID, error) {
	now := s.Clock.Now()
	if reuse != uuid.Nil {
		d, err := q.DeviceForReuse(ctx, sqlc.DeviceForReuseParams{ID: reuse, AccountID: acct})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return uuid.Nil, err
		case d.Platform != "web" && len(d.PublicKey) == ed25519.PublicKeySize && verifyProof(d.PublicKey, d.ID, in.Device.Proof):
			return d.ID, q.TouchDevice(ctx, sqlc.TouchDeviceParams{ID: d.ID, Model: in.Device.Model, AppVersion: in.Device.AppVersion, Now: &now})
		}
	}
	var pk []byte
	if pubKey != nil {
		pk = []byte(pubKey)
		// 同一账号未吊销的设备公钥不得重复（AUTH-10）：已有设备应通过设备证明复用。
		taken, err := q.ActiveDeviceKeyExists(ctx, sqlc.ActiveDeviceKeyExistsParams{AccountID: acct, PublicKey: pk})
		if err != nil {
			return uuid.Nil, err
		}
		if taken {
			return uuid.Nil, apierr.Invalid(apierr.Field("device.public_key", "not_allowed"))
		}
	}
	return q.InsertDevice(ctx, sqlc.InsertDeviceParams{
		AccountID: acct, Platform: in.Device.Platform, Model: in.Device.Model, AppVersion: in.Device.AppVersion, PublicKey: pk, Now: &now,
	})
}

// ProofMessage 是设备证明的签名对象（AUTH-10）：前缀与扫码批准的签名对象不同（域分隔），
// device_id 为小写、带连字符的 UUID，nonce 按接口返回值原样拼入。
func ProofMessage(device uuid.UUID, nonce string) []byte {
	return []byte("akari-device-proof-v1|" + device.String() + "|" + nonce)
}

func verifyProof(pub []byte, device uuid.UUID, p *Proof) bool {
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), ProofMessage(device, p.Nonce), sig)
}

// issueCredential 决定非 web 设备的凭据状态，必要时生成凭据（AUTH-13、AUTH-14）。
// 设备上限只统计未吊销的非 web 设备；免费账号的上限取设置项 free_device_limit（默认 1），
// 但没有生效中的权益时不下发凭据（spec/30 CredentialStatus）。
func (s *Service) issueCredential(ctx context.Context, q *sqlc.Queries, acct, device uuid.UUID) (string, error) {
	if _, err := q.LockAccount(ctx, acct); err != nil {
		return "", err
	}
	if _, err := q.DeviceCredential(ctx, &device); err == nil {
		return "issued", nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	e, err := q.CredentialEntitlement(ctx, acct)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && e.Status != "active") {
		return "entitlement_inactive", nil
	}
	if err != nil {
		return "", err
	}
	others, err := q.CountActiveDevices(ctx, sqlc.CountActiveDevicesParams{AccountID: acct, Exclude: device})
	if err != nil {
		return "", err
	}
	if others >= int64(e.DeviceLimit) {
		return "device_limit_reached", nil
	}
	secret := uuid.New()
	enc, err := s.Keys.Seal(secret[:], account.CredentialSecretAD)
	if err != nil {
		return "", err
	}
	id, err := q.CreateDeviceCredential(ctx, sqlc.CreateDeviceCredentialParams{AccountID: acct, DeviceID: &device, SecretEnc: enc})
	if err != nil {
		return "", err
	}
	return "issued", account.CredentialChanged(ctx, q, acct, id, "created")
}

// NewNonce 生成设备复用 nonce（AUTH-10）：32 字节随机值，60 秒有效，只能使用一次。
func (s *Service) NewNonce(ctx context.Context) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	n := base64.RawURLEncoding.EncodeToString(b)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.KV.Do(ctx, s.KV.B().Set().Key(nonceKey(n)).Value("1").Ex(NonceTTL).Build()).Error(); err != nil {
		return "", apierr.Unavailable(err)
	}
	return n, nil
}

func nonceKey(n string) string { return "auth:nonce:" + n }

// consumeNonce 原子地取出并删除 nonce；不存在（过期或已使用）时返回 false。
func (s *Service) consumeNonce(ctx context.Context, n string) bool {
	if n == "" || strings.ContainsAny(n, " \r\n") {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := s.KV.Do(ctx, s.KV.B().Getdel().Key(nonceKey(n)).Build()).ToString()
	return err == nil
}
