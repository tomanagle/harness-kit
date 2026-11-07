// Package store is the server's persistence boundary. Implementations commit
// before returning acknowledgements and validate scope on every operation.
package store

import (
	"context"
	"errors"

	"github.com/tomanagle/harness-kit/server/internal/wire"
)

var (
	ErrGone         = errors.New("gone")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrNotFound     = errors.New("not_found")
	ErrPending      = errors.New("authorization_pending")
	ErrExpired      = errors.New("expired_token")
	ErrSlowDown     = errors.New("slow_down")
	ErrInvalid      = errors.New("invalid_request")
)

type Principal struct{ AccountID, TenantID, ApplicationID, DeviceID, Role, CredentialHash string }
type Store interface {
	Register(context.Context, string, string) (wire.Account, error)
	Login(context.Context, string, string) (wire.Account, error)
	Authenticate(context.Context, string) (Principal, error)
	Tenants(context.Context, Principal) (wire.Tenants, error)
	CreateTenant(context.Context, Principal, wire.CreateTenant) (wire.Tenant, error)
	AddMember(context.Context, Principal, wire.AddMember) error
	RemoveMember(context.Context, Principal, wire.RemoveMember) error
	Begin(context.Context, wire.DeviceBegin) (wire.DeviceAuthorization, error)
	Inspect(context.Context, Principal, wire.DeviceInspect) (wire.DeviceInfo, error)
	Approve(context.Context, Principal, wire.DeviceApprove) error
	Poll(context.Context, wire.DevicePoll) (wire.Grant, error)
	Refresh(context.Context, wire.Refresh) (wire.Grant, error)
	Revoke(context.Context, Principal, wire.Revoke) error
	Push(context.Context, Principal, wire.Push) (wire.Ack, error)
	Changes(context.Context, Principal, wire.ChangesRequest) (wire.Changes, error)
	Events(context.Context, Principal, wire.EventsRequest) (wire.Events, error)
	UpdateSession(context.Context, Principal, wire.UpdateSession) (wire.Change, error)
	DeleteSession(context.Context, Principal, wire.DeleteSession) (wire.Change, error)
	AcquireLease(context.Context, Principal, wire.AcquireLease) (wire.Lease, error)
	RenewLease(context.Context, Principal, wire.RenewLease) (wire.Lease, error)
	ReleaseLease(context.Context, Principal, wire.RenewLease) (wire.Empty, error)
	SendCommand(context.Context, Principal, wire.SendCommand) (wire.Command, error)
	ClaimCommand(context.Context, Principal, wire.ClaimCommand) (wire.Commands, error)
	CompleteCommand(context.Context, Principal, wire.CompleteCommand) (wire.Command, error)
	GetCommand(context.Context, Principal, wire.GetCommand) (wire.Command, error)
	Members(context.Context, Principal, wire.TenantRequest) (wire.Members, error)
	SetMemberRole(context.Context, Principal, wire.SetMemberRole) (wire.Empty, error)
	Devices(context.Context, Principal, wire.Empty) (wire.Devices, error)
	Logout(context.Context, Principal, wire.Empty) (wire.Empty, error)
	RateLimit(context.Context, string, int) error
	GetSession(context.Context, Principal, wire.GetSession) (wire.Change, error)
	RequestEmail(context.Context, wire.RequestEmail) (wire.Empty, error)
	ConfirmEmail(context.Context, wire.ConfirmEmail) (wire.Empty, error)
	Ping(context.Context) error
}
