package dbstore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/tenants"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func (s *Store) Members(ctx context.Context, p store.Principal, in wire.TenantRequest) (wire.Members, error) {
	out := wire.Members{Members: []wire.Member{}}
	if err := requireAccount(p); err != nil {
		return out, err
	}
	var role string
	if err := s.db.QueryRow(ctx, "SELECT role FROM memberships WHERE tenant_id=$1 AND user_id=$2", in.TenantID, p.AccountID).Scan(&role); err != nil {
		return out, store.ErrForbidden
	}
	if !tenants.Allows(role, tenants.ManageMembers) {
		return out, store.ErrForbidden
	}
	rows, err := s.db.Query(ctx, "SELECT u.id,u.email,m.role FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 ORDER BY u.email LIMIT 1000", in.TenantID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var m wire.Member
		if err = rows.Scan(&m.AccountID, &m.Email, &m.Role); err != nil {
			return out, err
		}
		out.Members = append(out.Members, m)
	}
	return out, rows.Err()
}
func (s *Store) SetMemberRole(ctx context.Context, p store.Principal, in wire.SetMemberRole) (wire.Empty, error) {
	if err := requireAccount(p); err != nil {
		return wire.Empty{}, err
	}
	if in.Role != "admin" && in.Role != "member" && in.Role != "viewer" {
		return wire.Empty{}, store.ErrInvalid
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Empty, error) {
		rows, err := tx.Query(ctx, "SELECT user_id,role FROM memberships WHERE tenant_id=$1 AND (user_id=$2 OR user_id=$3) ORDER BY user_id FOR UPDATE", in.TenantID, p.AccountID, in.AccountID)
		if err != nil {
			return wire.Empty{}, err
		}
		roles := map[string]string{}
		for rows.Next() {
			var id, role string
			if err = rows.Scan(&id, &role); err != nil {
				rows.Close()
				return wire.Empty{}, err
			}
			roles[id] = role
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return wire.Empty{}, err
		}
		actor, target := roles[p.AccountID], roles[in.AccountID]
		if target == "" {
			return wire.Empty{}, store.ErrNotFound
		}
		if target == "owner" || !tenants.Allows(actor, tenants.ManageMembers) || (target == "admin" || in.Role == "admin") && !tenants.Allows(actor, tenants.ManageAdmins) {
			return wire.Empty{}, store.ErrForbidden
		}
		_, err = tx.Exec(ctx, "UPDATE memberships SET role=$3 WHERE tenant_id=$1 AND user_id=$2", in.TenantID, in.AccountID, in.Role)
		return wire.Empty{}, err
	})
}
func (s *Store) Devices(ctx context.Context, p store.Principal, _ wire.Empty) (wire.Devices, error) {
	out := wire.Devices{Devices: []wire.Device{}}
	if err := requireAccount(p); err != nil {
		return out, err
	}
	rows, err := s.db.Query(ctx, "SELECT id,tenant_id,application_id,name,revoked FROM devices WHERE user_id=$1 ORDER BY id LIMIT 1000", p.AccountID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var d wire.Device
		if err = rows.Scan(&d.ID, &d.TenantID, &d.ApplicationID, &d.Name, &d.Revoked); err != nil {
			return out, err
		}
		out.Devices = append(out.Devices, d)
	}
	return out, rows.Err()
}
func (s *Store) Logout(ctx context.Context, p store.Principal, _ wire.Empty) (wire.Empty, error) {
	if err := requireAccount(p); err != nil {
		return wire.Empty{}, err
	}
	_, err := s.db.Exec(ctx, "DELETE FROM account_tokens WHERE hash=$1 AND user_id=$2", p.CredentialHash, p.AccountID)
	return wire.Empty{}, err
}

// RateLimit is a deployment-wide fixed window, using the database clock. Local
// semaphores still bound per-process password memory; neither needs sticky routing.
func (s *Store) RateLimit(ctx context.Context, key string, limit int) error {
	var count int
	err := s.db.QueryRow(ctx, `INSERT INTO rate_limits(key,window_start,count) VALUES($1,clock_timestamp(),1) ON CONFLICT(key) DO UPDATE SET count=CASE WHEN rate_limits.window_start < clock_timestamp()-interval '1 minute' THEN 1 ELSE rate_limits.count+1 END, window_start=CASE WHEN rate_limits.window_start < clock_timestamp()-interval '1 minute' THEN clock_timestamp() ELSE rate_limits.window_start END RETURNING count`, key).Scan(&count)
	if err != nil {
		return err
	}
	if count > limit {
		return store.ErrSlowDown
	}
	return nil
}
