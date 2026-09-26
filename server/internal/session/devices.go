// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/account"
	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// RemoveDevice 移除本账号的一台设备（AUTH-15）：在同一事务中吊销设备、其全部会话与代理凭据（写 credential.changed），
// 再按设备上限重新分配凭据，等待中的设备随即取得空出的名额（AUTH-14）。web 设备同样可以移除；
// 移除当前设备等同于登出。不存在、已吊销或属于其他账号的设备返回 404（CONV-15）。
// 返回被移除的是否为当前会话所属的设备。
func (s *Service) RemoveDevice(ctx context.Context, p auth.Principal, device uuid.UUID) (bool, error) {
	var (
		revoked []uuid.UUID
		current bool
	)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		now := s.Clock.Now()
		if _, err := q.LockAccount(ctx, p.AccountID); err != nil {
			return err
		}
		if _, err := q.LockOwnDevice(ctx, sqlc.LockOwnDeviceParams{ID: device, AccountID: p.AccountID}); errors.Is(err, pgx.ErrNoRows) {
			return apierr.NotFound
		} else if err != nil {
			return err
		}
		cur, err := q.SessionDevice(ctx, sqlc.SessionDeviceParams{ID: p.SessionID, AccountID: p.AccountID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		current = cur != nil && *cur == device
		if err := q.RevokeDevice(ctx, sqlc.RevokeDeviceParams{ID: device, Now: &now}); err != nil {
			return err
		}
		if revoked, err = q.RevokeDeviceSessions(ctx, sqlc.RevokeDeviceSessionsParams{DeviceID: &device, Now: &now}); err != nil {
			return err
		}
		creds, err := q.RevokeDeviceCredentials(ctx, sqlc.RevokeDeviceCredentialsParams{DeviceID: &device, Now: &now})
		if err != nil {
			return err
		}
		for _, c := range creds {
			if err := credentialRevoked(ctx, q, p.AccountID, c); err != nil {
				return err
			}
		}
		return account.ReconcileCredentials(ctx, q, s.Keys, p.AccountID, now)
	})
	if err != nil {
		return false, err
	}
	if current {
		// 与登出相同：当前会话即使已不在未吊销集合中，也写入吊销集合，使访问令牌立即失效。
		revoked = append(revoked, p.SessionID)
	}
	if len(revoked) > 0 {
		if err := s.revokeAfterCommit(ctx, revoked); err != nil {
			return current, apierr.Unavailable(err)
		}
	}
	return current, nil
}
