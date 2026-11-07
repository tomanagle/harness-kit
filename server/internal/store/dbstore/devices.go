package dbstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func (s *Store) Begin(ctx context.Context, in wire.DeviceBegin) (wire.DeviceAuthorization, error) {
	if !validID(in.ApplicationID) || len(in.Name) < 1 || len(in.Name) > 200 {
		return wire.DeviceAuthorization{}, invalid("applicationId and device name are required")
	}
	code, err := s.tokens.Token()
	if err != nil {
		return wire.DeviceAuthorization{}, err
	}
	id, err := s.tokens.ID()
	if err != nil {
		return wire.DeviceAuthorization{}, err
	}
	if len(id) < 12 {
		return wire.DeviceAuthorization{}, errors.New("auth: generated ID is too short")
	}
	userCode := strings.ToUpper(id[:12])
	_, err = s.db.Exec(ctx, `INSERT INTO device_requests(hash,user_code,application_id,name,expires_at) VALUES($1,$2,$3,$4,now()+interval '10 minutes')`, auth.Digest(code), userCode, in.ApplicationID, in.Name)
	return wire.DeviceAuthorization{DeviceCode: code, UserCode: userCode, ExpiresIn: 600, Interval: 5}, err
}

func (s *Store) Approve(ctx context.Context, p store.Principal, in wire.DeviceApprove) error {
	if err := requireAccount(p); err != nil {
		return err
	}
	_, err := transaction(ctx, s, func(tx pgx.Tx) (wire.Empty, error) {
		if s.requireVerifiedEmail {
			var verified bool
			if err := tx.QueryRow(ctx, "SELECT email_verified FROM users WHERE id=$1", p.AccountID).Scan(&verified); err != nil {
				return wire.Empty{}, err
			}
			if !verified {
				return wire.Empty{}, store.ErrForbidden
			}
		}
		var member bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM memberships WHERE tenant_id=$1 AND user_id=$2)", in.TenantID, p.AccountID).Scan(&member); err != nil {
			return wire.Empty{}, err
		}
		if !member {
			return wire.Empty{}, store.ErrForbidden
		}
		var hash, app, name string
		err := tx.QueryRow(ctx, `SELECT hash,application_id,name FROM device_requests WHERE user_code=$1 AND expires_at>now() AND device_id IS NULL AND NOT consumed FOR UPDATE`, strings.ToUpper(strings.TrimSpace(in.UserCode))).Scan(&hash, &app, &name)
		if err != nil {
			return wire.Empty{}, err
		}
		id, err := s.tokens.ID()
		if err != nil {
			return wire.Empty{}, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO devices(id,user_id,tenant_id,application_id,name) VALUES($1,$2,$3,$4,$5)", id, p.AccountID, in.TenantID, app, name); err != nil {
			return wire.Empty{}, err
		}
		_, err = tx.Exec(ctx, "UPDATE device_requests SET device_id=$1 WHERE hash=$2", id, hash)
		return wire.Empty{}, err
	})
	return err
}

func (s *Store) issue(ctx context.Context, tx pgx.Tx, device string) (wire.Grant, error) {
	var g wire.Grant
	err := tx.QueryRow(ctx, `SELECT d.id,d.user_id,d.tenant_id,d.application_id FROM devices d JOIN memberships m ON m.user_id=d.user_id AND m.tenant_id=d.tenant_id WHERE d.id=$1 AND NOT d.revoked FOR SHARE OF d,m`, device).Scan(&g.DeviceID, &g.AccountID, &g.TenantID, &g.ApplicationID)
	if err != nil {
		return g, store.ErrUnauthorized
	}
	g.AccessToken, err = s.tokens.Token()
	if err != nil {
		return g, err
	}
	g.RefreshToken, err = s.tokens.Token()
	if err != nil {
		return g, err
	}
	expires := s.now().Add(15 * time.Minute)
	g.ExpiresAt = stamp(expires)
	if _, err = tx.Exec(ctx, "INSERT INTO access_tokens(hash,device_id,expires_at) VALUES($1,$2,$3)", auth.Digest(g.AccessToken), device, expires); err != nil {
		return g, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO refresh_tokens(hash,device_id,expires_at) VALUES($1,$2,now()+interval '30 days')`, auth.Digest(g.RefreshToken), device)
	return g, err
}

func (s *Store) Poll(ctx context.Context, in wire.DevicePoll) (wire.Grant, error) {
	// Pending/slow-down state must commit even though the protocol reports an error.
	var protocolError error
	g, err := transaction(ctx, s, func(tx pgx.Tx) (wire.Grant, error) {
		var device *string
		var expired, consumed, early bool
		err := tx.QueryRow(ctx, `SELECT device_id,expires_at<=now(),consumed,next_poll_at>now() FROM device_requests WHERE hash=$1 FOR UPDATE`, auth.Digest(in.DeviceCode)).Scan(&device, &expired, &consumed, &early)
		if err == pgx.ErrNoRows || expired || consumed {
			return wire.Grant{}, store.ErrExpired
		}
		if err != nil {
			return wire.Grant{}, err
		}
		if early {
			protocolError = store.ErrSlowDown
			return wire.Grant{}, nil
		}
		if _, err = tx.Exec(ctx, `UPDATE device_requests SET next_poll_at=now()+interval '5 seconds' WHERE hash=$1`, auth.Digest(in.DeviceCode)); err != nil {
			return wire.Grant{}, err
		}
		if device == nil {
			protocolError = store.ErrPending
			return wire.Grant{}, nil
		}
		g, err := s.issue(ctx, tx, *device)
		if err != nil {
			return g, err
		}
		_, err = tx.Exec(ctx, "UPDATE device_requests SET consumed=true WHERE hash=$1", auth.Digest(in.DeviceCode))
		return g, err
	})
	if err == nil {
		err = protocolError
	}
	return g, err
}

func (s *Store) Refresh(ctx context.Context, in wire.Refresh) (wire.Grant, error) {
	replay := false
	g, err := transaction(ctx, s, func(tx pgx.Tx) (wire.Grant, error) {
		var device string
		var used, expired bool
		err := tx.QueryRow(ctx, `SELECT device_id,used,expires_at<=now() FROM refresh_tokens WHERE hash=$1 FOR UPDATE`, auth.Digest(in.RefreshToken)).Scan(&device, &used, &expired)
		if err != nil {
			return wire.Grant{}, store.ErrUnauthorized
		}
		if used {
			replay = true
			_, err = tx.Exec(ctx, "UPDATE devices SET revoked=true WHERE id=$1", device)
			return wire.Grant{}, err
		}
		if expired {
			return wire.Grant{}, store.ErrUnauthorized
		}
		if _, err = tx.Exec(ctx, "UPDATE refresh_tokens SET used=true WHERE hash=$1", auth.Digest(in.RefreshToken)); err != nil {
			return wire.Grant{}, err
		}
		return s.issue(ctx, tx, device)
	})
	if replay && err == nil {
		err = store.ErrUnauthorized
	}
	return g, err
}

func (s *Store) Revoke(ctx context.Context, p store.Principal, in wire.Revoke) error {
	if p.DeviceID != "" && p.DeviceID != in.DeviceID {
		return store.ErrForbidden
	}
	tag, err := s.db.Exec(ctx, "UPDATE devices SET revoked=true WHERE id=$1 AND user_id=$2", in.DeviceID, p.AccountID)
	if err == nil && tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return err
}

func (s *Store) Inspect(ctx context.Context, p store.Principal, in wire.DeviceInspect) (wire.DeviceInfo, error) {
	var out wire.DeviceInfo
	if err := requireAccount(p); err != nil {
		return out, err
	}
	err := s.db.QueryRow(ctx, `SELECT application_id,name FROM device_requests WHERE user_code=$1 AND expires_at>now() AND device_id IS NULL AND NOT consumed`, strings.ToUpper(strings.TrimSpace(in.UserCode))).Scan(&out.ApplicationID, &out.Name)
	return out, dbError(err)
}
