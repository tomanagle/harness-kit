package tenants

import "testing"

func TestRoles(t *testing.T) {
	for _, tc := range []struct {
		role       string
		permission Permission
		want       bool
	}{{"owner", ManageAdmins, true}, {"admin", ManageAdmins, false}, {"admin", ManageMembers, true}, {"member", ManageMembers, false}, {"member", PublishSessions, true}, {"viewer", PublishSessions, false}, {"viewer", ReadSharedSessions, true}, {"unknown", ReadSharedSessions, false}, {"owner", Permission("unknown"), false}} {
		if Allows(tc.role, tc.permission) != tc.want {
			t.Errorf("%s %s", tc.role, tc.permission)
		}
	}
}
