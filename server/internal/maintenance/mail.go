package maintenance

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/tomanagle/harness-kit/server/internal/email"
)

// DeliverOne locks one queued message. Competing replicas skip it. Delivery may
// repeat after a post-SMTP commit failure, but links are single-use and identical.
func DeliverOne(ctx context.Context, db Database, sender email.Sender) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id string
	var m email.Message
	err = tx.QueryRow(ctx, `SELECT id,recipient,subject,body FROM mail_jobs WHERE available_at<=now() AND expires_at>now() AND attempts<10 ORDER BY available_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &m.To, &m.Subject, &m.Body)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = sender.Send(ctx, m); err != nil {
		if _, e := tx.Exec(ctx, "UPDATE mail_jobs SET attempts=attempts+1,available_at=now()+interval '1 minute' WHERE id=$1", id); e != nil {
			return false, e
		}
		if e := tx.Commit(ctx); e != nil {
			return false, e
		}
		return true, errors.New("email delivery failed; retry scheduled")
	}
	if _, err = tx.Exec(ctx, "DELETE FROM mail_jobs WHERE id=$1", id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
