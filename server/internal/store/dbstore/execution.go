package dbstore

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/tenants"
	"github.com/tomanagle/harness-kit/server/internal/wire"
	"time"
)

// Lock order matches uploads: grant/membership, feed head, session. PostgreSQL's
// clock is authoritative; replica clock skew cannot grant two active owners.
func (s *Store) executionSession(ctx context.Context, tx pgx.Tx, p store.Principal, id string) (wire.Lease, bool, error) {
	var out wire.Lease
	out.SessionID = id
	if !validID(id) {
		return out, false, store.ErrInvalid
	}
	if err := s.scope(ctx, tx, p); err != nil {
		return out, false, err
	}
	if !tenants.Allows(p.Role, tenants.PublishSessions) {
		return out, false, store.ErrForbidden
	}
	var cursor int64
	if err := tx.QueryRow(ctx, "SELECT cursor FROM sync_heads WHERE tenant_id=$1 AND application_id=$2 FOR UPDATE", p.TenantID, p.ApplicationID).Scan(&cursor); err != nil {
		return out, false, err
	}
	var owner string
	var deleted, active bool
	err := tx.QueryRow(ctx, `SELECT owner_id,writer_id,writer_epoch,deleted,COALESCE(lease_until>clock_timestamp(),false) FROM sessions WHERE tenant_id=$1 AND application_id=$2 AND id=$3 FOR UPDATE`, p.TenantID, p.ApplicationID, id).Scan(&owner, &out.DeviceID, &out.Epoch, &deleted, &active)
	if err != nil {
		return out, false, err
	}
	if owner != p.AccountID {
		return out, false, store.ErrForbidden
	}
	if deleted {
		return out, false, store.ErrGone
	}
	return out, active, nil
}
func (s *Store) AcquireLease(ctx context.Context, p store.Principal, in wire.AcquireLease) (wire.Lease, error) {
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Lease, error) {
		lease, active, err := s.executionSession(ctx, tx, p, in.SessionID)
		if err != nil {
			return lease, err
		}
		if lease.Epoch != in.ExpectedEpoch || lease.Epoch >= maxSafeInteger || active && lease.DeviceID != p.DeviceID {
			return lease, store.ErrConflict
		}
		// Only the existing writer can upgrade a legacy session to fenced execution.
		if lease.Epoch == 0 && lease.DeviceID != p.DeviceID {
			return lease, store.ErrForbidden
		}
		if !active {
			lease.Epoch++
			// Claim is at-most-once. A crashed worker's tool may have executed; never
			// replay it automatically on a different device after a lease expires.
			if _, err = tx.Exec(ctx, `UPDATE commands SET status='uncertain' WHERE tenant_id=$1 AND application_id=$2 AND session_id=$3 AND status IN ('pending','claimed')`, p.TenantID, p.ApplicationID, in.SessionID); err != nil {
				return lease, err
			}
		}
		lease.DeviceID = p.DeviceID
		var expires time.Time
		err = tx.QueryRow(ctx, `UPDATE sessions SET writer_id=$4,writer_epoch=$5,lease_until=clock_timestamp()+interval '30 seconds' WHERE tenant_id=$1 AND application_id=$2 AND id=$3 RETURNING lease_until`, p.TenantID, p.ApplicationID, in.SessionID, p.DeviceID, lease.Epoch).Scan(&expires)
		lease.ExpiresAt = stamp(expires)
		if err != nil {
			return lease, err
		}
		var cursor int64
		err = tx.QueryRow(ctx, `UPDATE sync_heads SET cursor=cursor+1 WHERE tenant_id=$1 AND application_id=$2 AND cursor<9007199254740991 RETURNING cursor`, p.TenantID, p.ApplicationID).Scan(&cursor)
		if err != nil {
			return lease, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO changes(tenant_id,application_id,cursor,session_id,last_seq) SELECT tenant_id,application_id,$4,id,last_seq FROM sessions WHERE tenant_id=$1 AND application_id=$2 AND id=$3`, p.TenantID, p.ApplicationID, in.SessionID, cursor)
		return lease, err
	})
}
func (s *Store) RenewLease(ctx context.Context, p store.Principal, in wire.RenewLease) (wire.Lease, error) {
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Lease, error) {
		lease, active, err := s.executionSession(ctx, tx, p, in.SessionID)
		if err != nil {
			return lease, err
		}
		if !active || lease.DeviceID != p.DeviceID || lease.Epoch != in.Epoch || in.Epoch < 1 {
			return lease, store.ErrConflict
		}
		var expires time.Time
		err = tx.QueryRow(ctx, `UPDATE sessions SET lease_until=clock_timestamp()+interval '30 seconds' WHERE tenant_id=$1 AND application_id=$2 AND id=$3 RETURNING lease_until`, p.TenantID, p.ApplicationID, in.SessionID).Scan(&expires)
		lease.ExpiresAt = stamp(expires)
		return lease, err
	})
}
func (s *Store) ReleaseLease(ctx context.Context, p store.Principal, in wire.RenewLease) (wire.Empty, error) {
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Empty, error) {
		lease, _, err := s.executionSession(ctx, tx, p, in.SessionID)
		if err != nil {
			return wire.Empty{}, err
		}
		if lease.DeviceID != p.DeviceID || lease.Epoch != in.Epoch || in.Epoch < 1 {
			return wire.Empty{}, store.ErrConflict
		}
		var busy bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE tenant_id=$1 AND application_id=$2 AND session_id=$3 AND status='claimed')`, p.TenantID, p.ApplicationID, in.SessionID).Scan(&busy); err != nil {
			return wire.Empty{}, err
		}
		if busy {
			return wire.Empty{}, store.ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE sessions SET lease_until=NULL WHERE tenant_id=$1 AND application_id=$2 AND id=$3`, p.TenantID, p.ApplicationID, in.SessionID)
		return wire.Empty{}, err
	})
}
func (s *Store) SendCommand(ctx context.Context, p store.Principal, in wire.SendCommand) (wire.Command, error) {
	raw, err := json.Marshal(in.Payload)
	if err != nil || len(raw) > 65536 || in.Payload == nil || !validID(in.ID) || (in.Kind != "prompt" && in.Kind != "cancel" && in.Kind != "approval") {
		return wire.Command{}, store.ErrInvalid
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Command, error) {
		lease, active, err := s.executionSession(ctx, tx, p, in.SessionID)
		if err != nil {
			return wire.Command{}, err
		}
		existing, err := readCommand(ctx, tx, p, in.ID)
		if err == nil {
			old, _ := json.Marshal(existing.Payload)
			if existing.SessionID != in.SessionID || existing.Kind != in.Kind || existing.Epoch != in.Epoch || string(old) != string(raw) {
				return existing, store.ErrConflict
			}
			return existing, nil
		}
		if err != pgx.ErrNoRows {
			return existing, err
		}
		if !active || lease.Epoch != in.Epoch || in.Epoch < 1 {
			return wire.Command{}, store.ErrConflict
		}
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM commands WHERE tenant_id=$1 AND application_id=$2 AND session_id=$3 AND status IN ('pending','claimed')`, p.TenantID, p.ApplicationID, in.SessionID).Scan(&count); err != nil {
			return wire.Command{}, err
		}
		if count >= 100 {
			return wire.Command{}, store.ErrSlowDown
		}
		_, err = tx.Exec(ctx, `INSERT INTO commands(tenant_id,application_id,session_id,id,requester_id,epoch,kind,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, p.TenantID, p.ApplicationID, in.SessionID, in.ID, p.AccountID, in.Epoch, in.Kind, in.Payload)
		if err != nil {
			return wire.Command{}, err
		}
		return readCommand(ctx, tx, p, in.ID)
	})
}
func readCommand(ctx context.Context, tx pgx.Tx, p store.Principal, id string) (wire.Command, error) {
	var out wire.Command
	err := tx.QueryRow(ctx, `SELECT id,session_id,epoch,kind,payload,status,result FROM commands WHERE tenant_id=$1 AND application_id=$2 AND id=$3 AND requester_id=$4`, p.TenantID, p.ApplicationID, id, p.AccountID).Scan(&out.ID, &out.SessionID, &out.Epoch, &out.Kind, &out.Payload, &out.Status, &out.Result)
	return out, err
}
func (s *Store) ClaimCommand(ctx context.Context, p store.Principal, in wire.ClaimCommand) (wire.Commands, error) {
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Commands, error) {
		out := wire.Commands{Commands: []wire.Command{}}
		lease, active, err := s.executionSession(ctx, tx, p, in.SessionID)
		if err != nil {
			return out, err
		}
		if !active || lease.DeviceID != p.DeviceID || lease.Epoch != in.Epoch {
			return out, store.ErrConflict
		}
		var id string
		err = tx.QueryRow(ctx, `UPDATE commands SET status='claimed' WHERE tenant_id=$1 AND application_id=$2 AND id=(SELECT id FROM commands WHERE tenant_id=$1 AND application_id=$2 AND session_id=$3 AND epoch=$4 AND status='pending' ORDER BY created_at,id LIMIT 1) RETURNING id`, p.TenantID, p.ApplicationID, in.SessionID, in.Epoch).Scan(&id)
		if err == pgx.ErrNoRows {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		command, err := readCommand(ctx, tx, p, id)
		out.Commands = append(out.Commands, command)
		return out, err
	})
}
func (s *Store) CompleteCommand(ctx context.Context, p store.Principal, in wire.CompleteCommand) (wire.Command, error) {
	raw, err := json.Marshal(in.Result)
	if err != nil || in.Result == nil || len(raw) > 65536 || (in.Status != "completed" && in.Status != "failed") {
		return wire.Command{}, store.ErrInvalid
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Command, error) {
		lease, active, err := s.executionSession(ctx, tx, p, in.SessionID)
		if err != nil {
			return wire.Command{}, err
		}
		if !active || lease.DeviceID != p.DeviceID || lease.Epoch != in.Epoch {
			return wire.Command{}, store.ErrConflict
		}
		command, err := readCommand(ctx, tx, p, in.ID)
		if err != nil {
			return command, err
		}
		if command.SessionID != in.SessionID || command.Epoch != in.Epoch {
			return command, store.ErrConflict
		}
		if command.Status != "claimed" {
			old, _ := json.Marshal(command.Result)
			if command.Status == in.Status && string(old) == string(raw) {
				return command, nil
			}
			return command, store.ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE commands SET status=$4,result=$5 WHERE tenant_id=$1 AND application_id=$2 AND id=$3`, p.TenantID, p.ApplicationID, in.ID, in.Status, in.Result)
		if err != nil {
			return command, err
		}
		return readCommand(ctx, tx, p, in.ID)
	})
}
func (s *Store) GetCommand(ctx context.Context, p store.Principal, in wire.GetCommand) (wire.Command, error) {
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Command, error) {
		if err := s.scope(ctx, tx, p); err != nil {
			return wire.Command{}, err
		}
		return readCommand(ctx, tx, p, in.ID)
	})
}
