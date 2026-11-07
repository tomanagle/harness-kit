package wire

import "reflect"

type Method struct {
	Name, Path    string
	Input, Output reflect.Type
}

func method[I, O any](name, path string) Method {
	return Method{name, path, reflect.TypeFor[I](), reflect.TypeFor[O]()}
}

var Methods = []Method{
	method[RequestEmail, Empty]("RequestEmail", "auth/request-email"),
	method[ConfirmEmail, Empty]("ConfirmEmail", "auth/confirm-email"),
	method[GetSession, Change]("GetSession", "sync/get"),
	method[TenantRequest, Members]("ListMembers", "tenants/members"),
	method[SetMemberRole, Empty]("SetMemberRole", "tenants/set-role"),
	method[Empty, Devices]("ListDevices", "devices/list"),
	method[Empty, Empty]("Logout", "auth/logout"),
	method[AcquireLease, Lease]("AcquireLease", "execution/acquire"),
	method[RenewLease, Lease]("RenewLease", "execution/renew"),
	method[RenewLease, Empty]("ReleaseLease", "execution/release"),
	method[SendCommand, Command]("SendCommand", "execution/send"),
	method[ClaimCommand, Commands]("ClaimCommand", "execution/claim"),
	method[CompleteCommand, Command]("CompleteCommand", "execution/complete"),
	method[GetCommand, Command]("GetCommand", "execution/get"),
	method[UpdateSession, Change]("UpdateSession", "sync/update"),
	method[DeleteSession, Change]("DeleteSession", "sync/delete"),
	method[Credentials, Account]("Register", "auth/register"), method[Credentials, Account]("Login", "auth/login"),
	method[Empty, Tenants]("ListTenants", "tenants/list"), method[CreateTenant, Tenant]("CreateTenant", "tenants/create"), method[AddMember, Empty]("AddMember", "tenants/add-member"),
	method[RemoveMember, Empty]("RemoveMember", "tenants/remove-member"),
	method[DeviceBegin, DeviceAuthorization]("BeginDevice", "devices/begin"), method[DeviceInspect, DeviceInfo]("InspectDevice", "devices/inspect"), method[DeviceApprove, Empty]("ApproveDevice", "devices/approve"), method[DevicePoll, Grant]("PollDevice", "devices/poll"), method[Refresh, Grant]("RefreshDevice", "devices/refresh"), method[Revoke, Empty]("RevokeDevice", "devices/revoke"),
	method[Push, Ack]("Push", "sync/push"), method[ChangesRequest, Changes]("Changes", "sync/changes"), method[EventsRequest, Events]("Events", "sync/events"),
}
