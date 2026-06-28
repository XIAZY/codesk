package notty

import "fmt"

const (
	MembershipRoleOwner  = "owner"
	MembershipRoleAdmin  = "admin"
	MembershipRoleMember = "member"
)

const (
	ActionInviteMembers   = "invite_members"   // POST /members
	ActionManageRoles     = "manage_roles"     // future: promote/demote permissions
	ActionManageAgents    = "manage_agents"    // create/update/delete agents
	ActionManageDaemons   = "manage_daemons"   // create/delete daemons
	ActionEditDocuments   = "edit_documents"   // create/update/delete documents
	ActionDeleteWorkspace = "delete_workspace" // workspace delete
)

var rolePermissions = map[string]map[string]struct{}{
	ActionInviteMembers:   {MembershipRoleOwner: {}, MembershipRoleAdmin: {}},
	ActionManageRoles:     {MembershipRoleOwner: {}},
	ActionManageAgents:    {MembershipRoleOwner: {}, MembershipRoleAdmin: {}},
	ActionManageDaemons:   {MembershipRoleOwner: {}, MembershipRoleAdmin: {}},
	ActionEditDocuments:   {MembershipRoleOwner: {}, MembershipRoleAdmin: {}, MembershipRoleMember: {}},
	ActionDeleteWorkspace: {MembershipRoleOwner: {}},
}

func validateMembershipRole(role string) error {
	switch role {
	case MembershipRoleOwner, MembershipRoleAdmin, MembershipRoleMember:
		return nil
	default:
		return fmt.Errorf("invalid membership role %q", role)
	}
}

func roleCanPerform(role string, action string) bool {
	allowed, ok := rolePermissions[action]
	if !ok {
		return false
	}
	_, ok = allowed[role]
	return ok
}

func requirePermission(auth *AuthContext, action string) error {
	if auth == nil {
		return fmt.Errorf("authentication required")
	}
	if auth.PrincipalKind == "daemon" {
		return nil
	}
	if !roleCanPerform(auth.MembershipRole, action) {
		return fmt.Errorf("insufficient permissions for action %q", action)
	}
	return nil
}
