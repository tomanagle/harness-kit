package dbstore

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/tenants"
	"github.com/tomanagle/harness-kit/server/internal/wire"
	"strings"
)

func (s *Store) UpdateSession(ctx context.Context, p store.Principal, in wire.UpdateSession) (wire.Change, error) {
	if len(in.Title) > 500 || len(in.Tags) > 100 || in.Metadata == nil || in.Tags == nil {
		return wire.Change{}, store.ErrInvalid
	}
	for _, tag := range in.Tags {
		if len(tag) > 100 || strings.TrimSpace(tag) == "" {
			return wire.Change{}, store.ErrInvalid
		}
	}
	encoded, err := json.Marshal(in.Metadata)
	if err != nil || len(encoded) > 65536 {
		return wire.Change{}, store.ErrInvalid
	}
	return s.metadata(ctx, p, in.OperationID, in.SessionID, in.ExpectedRevision, "update", in, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET title=$4,tags=$5,metadata=$6,archived=$7,revision=revision+1 WHERE tenant_id=$1 AND application_id=$2 AND id=$3`, p.TenantID, p.ApplicationID, in.SessionID, in.Title, in.Tags, in.Metadata, in.Archived)
		return err
	})
}
func (s *Store) DeleteSession(ctx context.Context, p store.Principal, in wire.DeleteSession) (wire.Change, error) {
	return s.metadata(ctx, p, in.OperationID, in.SessionID, in.ExpectedRevision, "delete", in, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET deleted=true,revision=revision+1 WHERE tenant_id=$1 AND application_id=$2 AND id=$3`, p.TenantID, p.ApplicationID, in.SessionID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM commands WHERE tenant_id=$1 AND application_id=$2 AND session_id=$3`, p.TenantID, p.ApplicationID, in.SessionID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM events WHERE tenant_id=$1 AND application_id=$2 AND session_id=$3`, p.TenantID, p.ApplicationID, in.SessionID)
		return err
	})
}
func (s *Store) metadata(ctx context.Context, p store.Principal, operation, id string, revision int64, kind string, payload any, apply func(pgx.Tx) error) (wire.Change, error) {
	if !validID(operation) || !validID(id) || revision < 1 || revision >= maxSafeInteger {
		return wire.Change{}, store.ErrInvalid
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return wire.Change{}, err
	}
	digest := auth.Digest(kind + ":" + string(body))
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Change, error) {
		var out wire.Change
		if err := s.scope(ctx, tx, p); err != nil {
			return out, err
		}
		if !tenants.Allows(p.Role, tenants.PublishSessions) {
			return out, store.ErrForbidden
		}
		var cursor int64
		if err := tx.QueryRow(ctx, "SELECT cursor FROM sync_heads WHERE tenant_id=$1 AND application_id=$2 FOR UPDATE", p.TenantID, p.ApplicationID).Scan(&cursor); err != nil {
			return out, err
		}
		var old string
		var raw []byte
		err := tx.QueryRow(ctx, "SELECT digest,ack FROM receipts WHERE tenant_id=$1 AND application_id=$2 AND device_id=$3 AND operation_id=$4", p.TenantID, p.ApplicationID, p.DeviceID, operation).Scan(&old, &raw)
		if err == nil {
			if old != digest {
				return out, store.ErrConflict
			}
			err = json.Unmarshal(raw, &out)
			return out, err
		}
		if err != pgx.ErrNoRows {
			return out, err
		}
		var owner string
		var current int64
		var deleted bool
		if err = tx.QueryRow(ctx, "SELECT owner_id,revision,deleted FROM sessions WHERE tenant_id=$1 AND application_id=$2 AND id=$3", p.TenantID, p.ApplicationID, id).Scan(&owner, &current, &deleted); err != nil {
			return out, err
		}
		if owner != p.AccountID {
			return out, store.ErrForbidden
		}
		if deleted {
			return out, store.ErrGone
		}
		if current != revision || cursor >= maxSafeInteger {
			return out, store.ErrConflict
		}
		if err = apply(tx); err != nil {
			return out, err
		}
		cursor++
		if _, err = tx.Exec(ctx, "UPDATE sync_heads SET cursor=$3 WHERE tenant_id=$1 AND application_id=$2", p.TenantID, p.ApplicationID, cursor); err != nil {
			return out, err
		}
		out.Cursor = cursor
		if err = tx.QueryRow(ctx, `SELECT id,last_seq,title,visibility,owner_id,revision,tags,metadata,archived,deleted,writer_id,writer_epoch FROM sessions WHERE tenant_id=$1 AND application_id=$2 AND id=$3`, p.TenantID, p.ApplicationID, id).Scan(&out.SessionID, &out.LastSeq, &out.Title, &out.Visibility, &out.OwnerID, &out.Revision, &out.Tags, &out.Metadata, &out.Archived, &out.Deleted, &out.WriterID, &out.WriterEpoch); err != nil {
			return out, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO changes(tenant_id,application_id,cursor,session_id,last_seq) VALUES($1,$2,$3,$4,$5)", p.TenantID, p.ApplicationID, cursor, id, out.LastSeq); err != nil {
			return out, err
		}
		raw, err = json.Marshal(out)
		if err != nil {
			return out, err
		}
		_, err = tx.Exec(ctx, "INSERT INTO receipts(tenant_id,application_id,device_id,operation_id,digest,ack) VALUES($1,$2,$3,$4,$5,$6)", p.TenantID, p.ApplicationID, p.DeviceID, operation, digest, raw)
		return out, err
	})
}

func (s *Store) GetSession(ctx context.Context, p store.Principal, in wire.GetSession) (wire.Change, error) {
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Change, error) {
		var out wire.Change
		if err := s.scope(ctx, tx, p); err != nil {
			return out, err
		}
		err := tx.QueryRow(ctx, `SELECT h.cursor,s.id,s.last_seq,s.title,s.visibility,s.owner_id,s.revision,s.tags,s.metadata,s.archived,s.deleted,s.writer_id,s.writer_epoch FROM sessions s JOIN sync_heads h ON h.tenant_id=s.tenant_id AND h.application_id=s.application_id WHERE s.tenant_id=$1 AND s.application_id=$2 AND s.id=$3 AND (s.owner_id=$4 OR s.visibility='shared')`, p.TenantID, p.ApplicationID, in.SessionID, p.AccountID).Scan(&out.Cursor, &out.SessionID, &out.LastSeq, &out.Title, &out.Visibility, &out.OwnerID, &out.Revision, &out.Tags, &out.Metadata, &out.Archived, &out.Deleted, &out.WriterID, &out.WriterEpoch)
		return out, err
	})
}
