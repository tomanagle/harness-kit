// Package tenants defines tenant roles in terms of named permissions. Resource
// ownership and visibility are checked separately; administrators do not gain
// access to somebody else's private session merely by being administrators.
package tenants

type Permission string

const (
	ReadSharedSessions Permission = "sessions.readShared"
	PublishSessions    Permission = "sessions.publish"
	ManageMembers      Permission = "members.manage"
	ManageAdmins       Permission = "members.manageAdmins"
)

func Allows(role string, permission Permission) bool {
	switch role {
	case "owner":
		return permission == ReadSharedSessions || permission == PublishSessions || permission == ManageMembers || permission == ManageAdmins
	case "admin":
		return permission == ReadSharedSessions || permission == PublishSessions || permission == ManageMembers
	case "member":
		return permission == ReadSharedSessions || permission == PublishSessions
	case "viewer":
		return permission == ReadSharedSessions
	default:
		return false
	}
}
