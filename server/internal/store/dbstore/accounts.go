package dbstore

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/store"
	"github.com/tomanagle/harness-kit/server/internal/tenants"
	"github.com/tomanagle/harness-kit/server/internal/wire"
)

func emailAddress(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || len(email) > 254 {
		return "", invalid("invalid email")
	}
	return email, nil
}

func (s *Store) Register(ctx context.Context, email, password string) (wire.Account, error) {
	email, err := emailAddress(email)
	if err != nil {
		return wire.Account{}, err
	}
	hash, err := s.passwords.Hash(password)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordInvalid) {
			return wire.Account{}, invalid(err.Error())
		}
		return wire.Account{}, err
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Account, error) {
		id, err := s.tokens.ID()
		if err != nil {
			return wire.Account{}, err
		}
		tenant, err := s.tokens.ID()
		if err != nil {
			return wire.Account{}, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,$3)", id, email, hash); err != nil {
			return wire.Account{}, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO tenants(id,name) VALUES($1,'Personal')", tenant); err != nil {
			return wire.Account{}, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')", tenant, id); err != nil {
			return wire.Account{}, err
		}
		if s.requireVerifiedEmail {
			if err := s.enqueueEmail(ctx, tx, id, email, "verify"); err != nil {
				return wire.Account{}, err
			}
		}
		return s.accountToken(ctx, tx, id)
	})
}

func (s *Store) accountToken(ctx context.Context, tx pgx.Tx, id string) (wire.Account, error) {
	token, err := s.tokens.Token()
	if err != nil {
		return wire.Account{}, err
	}
	expires := s.now().Add(12 * time.Hour)
	_, err = tx.Exec(ctx, "INSERT INTO account_tokens(hash,user_id,expires_at) VALUES($1,$2,$3)", auth.Digest(token), id, expires)
	return wire.Account{AccountID: id, AccessToken: token, ExpiresAt: stamp(expires)}, err
}

func (s *Store) Login(ctx context.Context, email, password string) (wire.Account, error) {
	email, err := emailAddress(email)
	if err != nil {
		return wire.Account{}, store.ErrUnauthorized
	}
	var id, hash string
	err = s.db.QueryRow(ctx, "SELECT id,password_hash FROM users WHERE email=$1", email).Scan(&id, &hash)
	if err != nil || !s.passwords.Verify(password, hash) {
		return wire.Account{}, store.ErrUnauthorized
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Account, error) { return s.accountToken(ctx, tx, id) })
}

func (s *Store) Authenticate(ctx context.Context, token string) (store.Principal, error) {
	if len(token) > 256 || token == "" {
		return store.Principal{}, store.ErrUnauthorized
	}
	p := store.Principal{CredentialHash: auth.Digest(token)}
	err := s.db.QueryRow(ctx, "SELECT user_id FROM account_tokens WHERE hash=$1 AND expires_at>now()", auth.Digest(token)).Scan(&p.AccountID)
	if err == nil {
		return p, nil
	}
	if err != pgx.ErrNoRows {
		return p, err
	}
	err = s.db.QueryRow(ctx, `SELECT d.user_id,d.tenant_id,d.application_id,d.id,m.role FROM access_tokens t JOIN devices d ON d.id=t.device_id JOIN memberships m ON m.user_id=d.user_id AND m.tenant_id=d.tenant_id WHERE t.hash=$1 AND t.expires_at>now() AND NOT d.revoked`, auth.Digest(token)).Scan(&p.AccountID, &p.TenantID, &p.ApplicationID, &p.DeviceID, &p.Role)
	if err == pgx.ErrNoRows {
		return store.Principal{}, store.ErrUnauthorized
	}
	return p, err
}

func (s *Store) Tenants(ctx context.Context, p store.Principal) (wire.Tenants, error) {
	out := wire.Tenants{Tenants: []wire.Tenant{}}
	if err := requireAccount(p); err != nil {
		return out, err
	}
	rows, err := s.db.Query(ctx, "SELECT t.id,t.name,m.role FROM memberships m JOIN tenants t ON t.id=m.tenant_id WHERE m.user_id=$1 ORDER BY t.id", p.AccountID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var t wire.Tenant
		if err = rows.Scan(&t.ID, &t.Name, &t.Role); err != nil {
			return out, err
		}
		out.Tenants = append(out.Tenants, t)
	}
	return out, rows.Err()
}

func (s *Store) CreateTenant(ctx context.Context, p store.Principal, in wire.CreateTenant) (wire.Tenant, error) {
	if err := requireAccount(p); err != nil {
		return wire.Tenant{}, err
	}
	if len(strings.TrimSpace(in.Name)) == 0 || len(in.Name) > 200 {
		return wire.Tenant{}, invalid("tenant name is required, maximum 200 bytes")
	}
	return transaction(ctx, s, func(tx pgx.Tx) (wire.Tenant, error) {
		id, err := s.tokens.ID()
		if err != nil {
			return wire.Tenant{}, err
		}
		t := wire.Tenant{ID: id, Name: strings.TrimSpace(in.Name), Role: "owner"}
		if _, err := tx.Exec(ctx, "INSERT INTO tenants(id,name) VALUES($1,$2)", t.ID, t.Name); err != nil {
			return t, err
		}
		_, err = tx.Exec(ctx, "INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'owner')", t.ID, p.AccountID)
		return t, err
	})
}

func (s *Store) AddMember(ctx context.Context, p store.Principal, in wire.AddMember) error {
	if err := requireAccount(p); err != nil {
		return err
	}
	if in.Role != "admin" && in.Role != "member" && in.Role != "viewer" {
		return invalid("role must be admin, member or viewer")
	}
	email, err := emailAddress(in.Email)
	if err != nil {
		return err
	}
	_, err = transaction(ctx, s, func(tx pgx.Tx) (wire.Empty, error) {
		var role string
		err := tx.QueryRow(ctx, "SELECT role FROM memberships WHERE tenant_id=$1 AND user_id=$2 FOR SHARE", in.TenantID, p.AccountID).Scan(&role)
		if err != nil || !tenants.Allows(role, tenants.ManageMembers) || in.Role == "admin" && !tenants.Allows(role, tenants.ManageAdmins) {
			return wire.Empty{}, store.ErrForbidden
		}
		tag, err := tx.Exec(ctx, "INSERT INTO memberships(tenant_id,user_id,role) SELECT $1,id,$3 FROM users WHERE email=$2", in.TenantID, email, in.Role)
		if err == nil && tag.RowsAffected() == 0 {
			err = store.ErrNotFound
		}
		return wire.Empty{}, err
	})
	return err
}

func (s *Store) RemoveMember(ctx context.Context, p store.Principal, in wire.RemoveMember) error {
	if err := requireAccount(p); err != nil {
		return err
	}
	_, err := transaction(ctx, s, func(tx pgx.Tx) (wire.Empty, error) {
		// Lock in a consistent order even if two administrators act on each other.
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
		if !tenants.Allows(actor, tenants.ManageMembers) || target == "owner" || target == "admin" && !tenants.Allows(actor, tenants.ManageAdmins) {
			return wire.Empty{}, store.ErrForbidden
		}
		if target == "" {
			return wire.Empty{}, store.ErrNotFound
		}
		_, err = tx.Exec(ctx, "DELETE FROM memberships WHERE tenant_id=$1 AND user_id=$2", in.TenantID, in.AccountID)
		return wire.Empty{}, err
	})
	return err
}
