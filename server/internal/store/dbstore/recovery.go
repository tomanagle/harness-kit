package dbstore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/wire"
	"net/url"
)

func (s *Store) enqueueEmail(ctx context.Context, tx pgx.Tx, id, email, purpose string) error {
	if !s.mailEnabled || s.publicURL == "" {
		return store.ErrInvalid
	}
	token, err := s.tokens.Token()
	if err != nil {
		return err
	}
	job, err := s.tokens.ID()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO account_challenges(hash,user_id,purpose,expires_at) VALUES($1,$2,$3,now()+interval '30 minutes')`, auth.Digest(token), id, purpose); err != nil {
		return err
	}
	subject := "Verify your email"
	if purpose == "reset" {
		subject = "Reset your password"
	}
	_, err = tx.Exec(ctx, `INSERT INTO mail_jobs(id,recipient,subject,body,expires_at) VALUES($1,$2,$3,$4,now()+interval '30 minutes')`, job, email, subject, "Open this link within 30 minutes:\n"+s.publicURL+"/account/confirm?token="+url.QueryEscape(token)+"&purpose="+purpose+"\nIf you did not request this, ignore this message.")
	return err
}
func (s *Store) RequestEmail(ctx context.Context, in wire.RequestEmail) (wire.Empty, error) {
	if !s.mailEnabled || (in.Purpose != "verify" && in.Purpose != "reset") {
		return wire.Empty{}, store.ErrInvalid
	}
	email, err := emailAddress(in.Email)
	if err != nil {
		return wire.Empty{}, err
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Empty, error) {
		var id string
		err := tx.QueryRow(ctx, "SELECT id FROM users WHERE email=$1 FOR UPDATE", email).Scan(&id)
		if err == pgx.ErrNoRows {
			return wire.Empty{}, nil
		}
		if err != nil {
			return wire.Empty{}, err
		}
		var recent bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM account_challenges WHERE user_id=$1 AND purpose=$2 AND created_at>now()-interval '1 minute')", id, in.Purpose).Scan(&recent); err != nil {
			return wire.Empty{}, err
		}
		if recent {
			return wire.Empty{}, nil
		}
		return wire.Empty{}, s.enqueueEmail(ctx, tx, id, email, in.Purpose)
	})
}
func (s *Store) ConfirmEmail(ctx context.Context, in wire.ConfirmEmail) (wire.Empty, error) {
	if len(in.Token) < 16 || len(in.Token) > 256 {
		return wire.Empty{}, store.ErrUnauthorized
	}
	hash := ""
	if in.Password != "" {
		var err error
		hash, err = s.passwords.Hash(in.Password)
		if err != nil {
			return wire.Empty{}, store.ErrInvalid
		}
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Empty, error) {
		var id, purpose string
		err := tx.QueryRow(ctx, "SELECT user_id,purpose FROM account_challenges WHERE hash=$1 AND expires_at>now() AND NOT used", auth.Digest(in.Token)).Scan(&id, &purpose)
		if err != nil {
			return wire.Empty{}, store.ErrUnauthorized
		}
		// All recovery mutations lock user before challenges, including resets
		// invalidating several outstanding links, to avoid cross-link deadlocks.
		if err = tx.QueryRow(ctx, "SELECT id FROM users WHERE id=$1 FOR UPDATE", id).Scan(&id); err != nil {
			return wire.Empty{}, err
		}
		if err = tx.QueryRow(ctx, "SELECT purpose FROM account_challenges WHERE hash=$1 AND expires_at>now() AND NOT used FOR UPDATE", auth.Digest(in.Token)).Scan(&purpose); err != nil {
			return wire.Empty{}, store.ErrUnauthorized
		}
		if purpose == "reset" {
			if hash == "" {
				return wire.Empty{}, store.ErrInvalid
			}
			if _, err = tx.Exec(ctx, "UPDATE users SET password_hash=$2,email_verified=true WHERE id=$1", id, hash); err != nil {
				return wire.Empty{}, err
			}
			if _, err = tx.Exec(ctx, "DELETE FROM account_tokens WHERE user_id=$1", id); err != nil {
				return wire.Empty{}, err
			}
			if _, err = tx.Exec(ctx, "UPDATE devices SET revoked=true WHERE user_id=$1", id); err != nil {
				return wire.Empty{}, err
			}
			// A reset invalidates all outstanding reset links, including concurrently requested ones.
			if _, err = tx.Exec(ctx, "UPDATE account_challenges SET used=true WHERE user_id=$1", id); err != nil {
				return wire.Empty{}, err
			}
		} else {
			if _, err = tx.Exec(ctx, "UPDATE users SET email_verified=true WHERE id=$1", id); err != nil {
				return wire.Empty{}, err
			}
		}
		_, err = tx.Exec(ctx, "UPDATE account_challenges SET used=true WHERE hash=$1", auth.Digest(in.Token))
		return wire.Empty{}, err
	})
}
