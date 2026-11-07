package dbstore

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/tenants"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

const maxSafeInteger int64 = 9007199254740991

func (s *Store) Push(ctx context.Context, p store.Principal, in wire.Push) (wire.Ack, error) {
	if in.Epoch < 0 || in.Epoch > maxSafeInteger || !validID(in.OperationID) || !validID(in.SessionID) || in.ExpectedSeq < 0 || in.ExpectedSeq > maxSafeInteger-100 || len(in.Title) > 500 || len(in.Events) > 100 || in.Visibility != "private" && in.Visibility != "shared" {
		return wire.Ack{}, invalid("invalid session batch")
	}
	for _, e := range in.Events {
		if len(e) > 65536 || !json.Valid(e) || len(e) == 0 || e[0] != '{' {
			return wire.Ack{}, invalid("events must be JSON objects of at most 64 KiB")
		}
	}
	body, err := json.Marshal(in)
	if err != nil || len(body) > 1048576 {
		return wire.Ack{}, invalid("batch exceeds 1 MiB")
	}
	digest := auth.Digest(string(body))
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Ack, error) {
		if err := s.scope(ctx, tx, p); err != nil {
			return wire.Ack{}, err
		}
		if !tenants.Allows(p.Role, tenants.PublishSessions) {
			return wire.Ack{}, store.ErrForbidden
		}
		// All writes for this tenant/application serialize through the head row. Unlike
		// a sequence, this cursor cannot become visible before an earlier commit.
		if _, err := tx.Exec(ctx, "INSERT INTO sync_heads(tenant_id,application_id) VALUES($1,$2) ON CONFLICT DO NOTHING", p.TenantID, p.ApplicationID); err != nil {
			return wire.Ack{}, err
		}
		var cursor int64
		if err := tx.QueryRow(ctx, "SELECT cursor FROM sync_heads WHERE tenant_id=$1 AND application_id=$2 FOR UPDATE", p.TenantID, p.ApplicationID).Scan(&cursor); err != nil {
			return wire.Ack{}, err
		}
		var previous string
		var ack wire.Ack
		var raw []byte
		err := tx.QueryRow(ctx, "SELECT digest,ack FROM receipts WHERE tenant_id=$1 AND application_id=$2 AND device_id=$3 AND operation_id=$4", p.TenantID, p.ApplicationID, p.DeviceID, in.OperationID).Scan(&previous, &raw)
		if err == nil {
			if previous != digest {
				return ack, store.ErrConflict
			}
			err = json.Unmarshal(raw, &ack)
			return ack, err
		}
		if err != pgx.ErrNoRows {
			return ack, err
		}
		var seq int64
		var writer, title, visibility string
		var deleted bool
		var epoch int64
		var leaseActive bool
		err = tx.QueryRow(ctx, "SELECT last_seq,writer_id,title,visibility,deleted,writer_epoch,COALESCE(lease_until>clock_timestamp(),false) FROM sessions WHERE tenant_id=$1 AND application_id=$2 AND id=$3", p.TenantID, p.ApplicationID, in.SessionID).Scan(&seq, &writer, &title, &visibility, &deleted, &epoch, &leaseActive)
		if err == pgx.ErrNoRows {
			if in.ExpectedSeq != 0 || in.Epoch != 0 {
				return ack, store.ErrConflict
			}
			_, err = tx.Exec(ctx, "INSERT INTO sessions(tenant_id,application_id,id,owner_id,writer_id,title,visibility) VALUES($1,$2,$3,$4,$5,$6,$7)", p.TenantID, p.ApplicationID, in.SessionID, p.AccountID, p.DeviceID, in.Title, in.Visibility)
		} else if err == nil {
			if deleted {
				return ack, store.ErrGone
			}
			if epoch != in.Epoch || epoch > 0 && !leaseActive {
				return ack, store.ErrConflict
			}
			if writer != p.DeviceID {
				return ack, store.ErrForbidden
			}
			if seq != in.ExpectedSeq || visibility != in.Visibility {
				return ack, store.ErrConflict
			}
		}
		if err != nil {
			return ack, err
		}
		for _, event := range in.Events {
			seq++
			if _, err = tx.Exec(ctx, "INSERT INTO events(tenant_id,application_id,session_id,seq,data) VALUES($1,$2,$3,$4,$5)", p.TenantID, p.ApplicationID, in.SessionID, seq, []byte(event)); err != nil {
				return ack, err
			}
		}
		if cursor >= maxSafeInteger {
			return ack, store.ErrConflict
		}
		cursor++
		if _, err = tx.Exec(ctx, "UPDATE sessions SET last_seq=$4 WHERE tenant_id=$1 AND application_id=$2 AND id=$3", p.TenantID, p.ApplicationID, in.SessionID, seq); err != nil {
			return ack, err
		}
		if _, err = tx.Exec(ctx, "UPDATE sync_heads SET cursor=$3 WHERE tenant_id=$1 AND application_id=$2", p.TenantID, p.ApplicationID, cursor); err != nil {
			return ack, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO changes(tenant_id,application_id,cursor,session_id,last_seq) VALUES($1,$2,$3,$4,$5)", p.TenantID, p.ApplicationID, cursor, in.SessionID, seq); err != nil {
			return ack, err
		}
		ack = wire.Ack{SessionID: in.SessionID, LastSeq: seq, Cursor: cursor}
		raw, err = json.Marshal(ack)
		if err != nil {
			return ack, err
		}
		_, err = tx.Exec(ctx, "INSERT INTO receipts(tenant_id,application_id,device_id,operation_id,digest,ack) VALUES($1,$2,$3,$4,$5,$6)", p.TenantID, p.ApplicationID, p.DeviceID, in.OperationID, digest, raw)
		return ack, err
	})
}

func pageLimit(n int) (int, error) {
	if n == 0 {
		return 100, nil
	}
	if n < 1 || n > 100 {
		return 0, invalid("limit must be between 1 and 100")
	}
	return n, nil
}

func (s *Store) Changes(ctx context.Context, p store.Principal, in wire.ChangesRequest) (wire.Changes, error) {
	limit, err := pageLimit(in.Limit)
	if err != nil {
		return wire.Changes{}, err
	}
	if in.After < 0 || in.After > maxSafeInteger {
		return wire.Changes{}, invalid("invalid cursor")
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Changes, error) {
		out := wire.Changes{Changes: []wire.Change{}}
		if err := s.scope(ctx, tx, p); err != nil {
			return out, err
		}
		var head int64
		err := tx.QueryRow(ctx, "SELECT cursor FROM sync_heads WHERE tenant_id=$1 AND application_id=$2", p.TenantID, p.ApplicationID).Scan(&head)
		if err != nil && err != pgx.ErrNoRows {
			return out, err
		}
		if in.After > head {
			return out, store.ErrConflict
		}
		rows, err := tx.Query(ctx, `SELECT c.cursor,c.session_id,c.last_seq,s.title,s.visibility,s.owner_id,s.revision,s.tags,s.metadata,s.archived,s.deleted,s.writer_id,s.writer_epoch FROM changes c JOIN sessions s ON s.tenant_id=c.tenant_id AND s.application_id=c.application_id AND s.id=c.session_id WHERE c.tenant_id=$1 AND c.application_id=$2 AND c.cursor>$3 AND c.cursor<=$4 AND (s.owner_id=$5 OR s.visibility='shared') ORDER BY c.cursor LIMIT $6`, p.TenantID, p.ApplicationID, in.After, head, p.AccountID, limit)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		for rows.Next() {
			var c wire.Change
			if err := rows.Scan(&c.Cursor, &c.SessionID, &c.LastSeq, &c.Title, &c.Visibility, &c.OwnerID, &c.Revision, &c.Tags, &c.Metadata, &c.Archived, &c.Deleted, &c.WriterID, &c.WriterEpoch); err != nil {
				return out, err
			}
			out.Changes = append(out.Changes, c)
		}
		out.Cursor = head
		if len(out.Changes) == limit {
			out.Cursor = out.Changes[len(out.Changes)-1].Cursor
		}
		return out, rows.Err()
	})
}

func (s *Store) Events(ctx context.Context, p store.Principal, in wire.EventsRequest) (wire.Events, error) {
	limit, err := pageLimit(in.Limit)
	if err != nil {
		return wire.Events{}, err
	}
	if !validID(in.SessionID) || in.After < 0 || in.After > maxSafeInteger {
		return wire.Events{}, invalid("invalid event cursor")
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Events, error) {
		out := wire.Events{Events: []wire.Event{}, NextAfter: in.After}
		if err := s.scope(ctx, tx, p); err != nil {
			return out, err
		}
		var deleted bool
		err := tx.QueryRow(ctx, "SELECT last_seq,deleted FROM sessions WHERE tenant_id=$1 AND application_id=$2 AND id=$3 AND (owner_id=$4 OR visibility='shared')", p.TenantID, p.ApplicationID, in.SessionID, p.AccountID).Scan(&out.LastSeq, &deleted)
		if err != nil {
			return out, err
		}
		if deleted {
			return out, store.ErrGone
		}
		if in.After > out.LastSeq {
			return out, store.ErrConflict
		}
		rows, err := tx.Query(ctx, "SELECT seq,data FROM events WHERE tenant_id=$1 AND application_id=$2 AND session_id=$3 AND seq>$4 AND seq<=$5 ORDER BY seq LIMIT $6", p.TenantID, p.ApplicationID, in.SessionID, in.After, out.LastSeq, limit)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		size := 0
		for rows.Next() {
			var e wire.Event
			if err := rows.Scan(&e.Seq, &e.Data); err != nil {
				return out, err
			}
			if size+len(e.Data) > 1048576 && len(out.Events) > 0 {
				break
			}
			size += len(e.Data)
			out.Events = append(out.Events, e)
			out.NextAfter = e.Seq
		}
		out.HasMore = out.NextAfter < out.LastSeq
		return out, rows.Err()
	})
}
