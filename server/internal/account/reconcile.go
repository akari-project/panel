// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
)

// 设备凭据的状态（spec/30 CredentialStatus）。
const (
	StatusIssued              = "issued"
	StatusDeviceLimitReached  = "device_limit_reached"
	StatusEntitlementInactive = "entitlement_inactive"
	StatusWebDevice           = "web_device"
)

// ReconcileCredentials 按设备上限分配账号的设备凭据（AUTH-13、AUTH-14），是唯一的分配入口：
// 登录、登出、移除设备与权益变化都在各自的事务中调用。先锁账号行，再锁设备行。
//
// 名额由“持有未吊销凭据的未吊销非 web 设备”占用；上限取当前权益的快照 device_limit。分配是单调的：
//   - 持有数超过上限（权益变化使上限降低）：按 last_seen_at 从远到近吊销凭据，直到等于上限；设备记录保留；
//   - 权益为 active 且持有数低于上限：按 last_seen_at 从近到远（相同时按 id）为尚无凭据的设备生成凭据，直到等于上限；
//   - 已持有凭据的设备不会因新设备而失去凭据（登录不抢占）；
//   - 没有当前权益或权益不是 active 时不吊销（到期与超额由 BIL-13、ACS-02 处理），也不生成；
//   - 不触碰共用凭据（device_id 为空）。
//
// 每条凭据的生成与吊销都写 credential.changed（CONV-34）。
func ReconcileCredentials(ctx context.Context, q *sqlc.Queries, keys *secretbox.Keyring, acct uuid.UUID, now time.Time) error {
	if _, err := q.LockAccount(ctx, acct); err != nil {
		return err
	}
	e, err := q.CredentialEntitlement(ctx, acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	slots, err := q.CredentialSlots(ctx, acct)
	if err != nil {
		return err
	}
	limit, held := int(e.DeviceLimit), 0
	for _, s := range slots {
		if s.CredentialID == nil {
			continue
		}
		if held < limit {
			held++
			continue
		}
		if err := q.RevokeCredential(ctx, sqlc.RevokeCredentialParams{ID: *s.CredentialID, Now: &now}); err != nil {
			return err
		}
		if err := CredentialChanged(ctx, q, acct, *s.CredentialID, "revoked"); err != nil {
			return err
		}
	}
	if e.Status != "active" {
		return nil
	}
	for _, s := range slots {
		if held >= limit {
			break
		}
		if s.CredentialID != nil {
			continue
		}
		if err := createDeviceCredential(ctx, q, keys, acct, s.ID); err != nil {
			return err
		}
		held++
	}
	return nil
}

func createDeviceCredential(ctx context.Context, q *sqlc.Queries, keys *secretbox.Keyring, acct, device uuid.UUID) error {
	secret := uuid.New()
	enc, err := keys.Seal(secret[:], CredentialSecretAD)
	if err != nil {
		return err
	}
	id, err := q.CreateDeviceCredential(ctx, sqlc.CreateDeviceCredentialParams{AccountID: acct, DeviceID: &device, SecretEnc: enc})
	if err != nil {
		return err
	}
	return CredentialChanged(ctx, q, acct, id, "created")
}

// DeviceCredentialStatus 返回非 web 设备的凭据状态，在 ReconcileCredentials 之后调用。
// 权益优先：没有当前权益或权益不是 active 时为 entitlement_inactive，即使设备仍持有凭据；
// 否则持有凭据为 issued，没有为 device_limit_reached。
func DeviceCredentialStatus(ctx context.Context, q *sqlc.Queries, acct, device uuid.UUID) (string, error) {
	e, err := q.CredentialEntitlement(ctx, acct)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && e.Status != "active") {
		return StatusEntitlementInactive, nil
	}
	if err != nil {
		return "", err
	}
	if _, err := q.DeviceCredential(ctx, &device); errors.Is(err, pgx.ErrNoRows) {
		return StatusDeviceLimitReached, nil
	} else if err != nil {
		return "", err
	}
	return StatusIssued, nil
}
