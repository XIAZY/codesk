package notty

import "testing"

func TestValidateMembershipRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role  string
		valid bool
	}{
		{MembershipRoleOwner, true},
		{MembershipRoleAdmin, true},
		{MembershipRoleMember, true},
		{"superadmin", false},
		{"", false},
	}
	for _, tc := range cases {
		err := validateMembershipRole(tc.role)
		if tc.valid && err != nil {
			t.Fatalf("role %q should be valid", tc.role)
		}
		if !tc.valid && err == nil {
			t.Fatalf("role %q should be invalid", tc.role)
		}
	}
}

func TestRoleCanPerform(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role     string
		action   string
		expected bool
	}{
		{MembershipRoleOwner, ActionInviteMembers, true},
		{MembershipRoleOwner, ActionManageRoles, true},
		{MembershipRoleOwner, ActionManageAgents, true},
		{MembershipRoleOwner, ActionManageDaemons, true},
		{MembershipRoleOwner, ActionEditDocuments, true},
		{MembershipRoleOwner, ActionDeleteWorkspace, true},
		{MembershipRoleAdmin, ActionInviteMembers, true},
		{MembershipRoleAdmin, ActionManageRoles, false},
		{MembershipRoleAdmin, ActionManageAgents, true},
		{MembershipRoleAdmin, ActionManageDaemons, true},
		{MembershipRoleAdmin, ActionEditDocuments, true},
		{MembershipRoleAdmin, ActionDeleteWorkspace, false},
		{MembershipRoleMember, ActionInviteMembers, false},
		{MembershipRoleMember, ActionManageRoles, false},
		{MembershipRoleMember, ActionManageAgents, false},
		{MembershipRoleMember, ActionManageDaemons, false},
		{MembershipRoleMember, ActionEditDocuments, true},
		{MembershipRoleMember, ActionDeleteWorkspace, false},
	}
	for _, tc := range cases {
		if got := roleCanPerform(tc.role, tc.action); got != tc.expected {
			t.Fatalf("role %q action %q expected %v got %v", tc.role, tc.action, tc.expected, got)
		}
	}
}

func TestRequirePermission(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		auth    *AuthContext
		action  string
		allowed bool
	}{
		{name: "owner", auth: &AuthContext{PrincipalKind: "human", MembershipRole: MembershipRoleOwner}, action: ActionManageAgents, allowed: true},
		{name: "admin", auth: &AuthContext{PrincipalKind: "human", MembershipRole: MembershipRoleAdmin}, action: ActionManageDaemons, allowed: true},
		{name: "member", auth: &AuthContext{PrincipalKind: "human", MembershipRole: MembershipRoleMember}, action: ActionManageAgents, allowed: false},
		{name: "member edit documents", auth: &AuthContext{PrincipalKind: "human", MembershipRole: MembershipRoleMember}, action: ActionEditDocuments, allowed: true},
		{name: "daemon bypass", auth: &AuthContext{PrincipalKind: "daemon", MembershipRole: "member"}, action: ActionManageAgents, allowed: true},
		{name: "nil auth", auth: nil, action: ActionManageAgents, allowed: false},
	}
	for _, tc := range cases {
		err := requirePermission(tc.auth, tc.action)
		if tc.allowed && err != nil {
			t.Fatalf("%s should allow action %q got %v", tc.name, tc.action, err)
		}
		if !tc.allowed && err == nil {
			t.Fatalf("%s should deny action %q", tc.name, tc.action)
		}
	}
}
