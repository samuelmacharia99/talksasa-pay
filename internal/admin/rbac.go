package admin

import "github.com/talksasa/talksasa-pay/internal/store"

// Permission is a capability checked per admin API route.
type Permission string

const (
	// PermRead allows viewing every screen: apps, charges, transactions,
	// unmatched payments, audit log, dashboard.
	PermRead Permission = "read"
	// PermManageApps allows creating apps, changing webhooks,
	// rotating webhook secrets and issuing/revoking API keys.
	PermManageApps Permission = "apps.manage"
	// PermManageUsers allows inviting, disabling and re-roling staff.
	PermManageUsers Permission = "users.manage"
	// PermResolvePayments allows confirming (with the M-Pesa receipt from
	// the portal) or failing an M-Pesa payment in needs_review. Confirming
	// records money, so it is limited to finance and admin (user decision
	// 2026-11-24); every resolution is audited with a reason.
	//
	// It also allows assigning an unmatched payment to an app (audited).
	PermResolvePayments Permission = "payments.resolve"
	// PermReversePayments allows recording a reversal of a succeeded
	// payment (money rule 9: admin only, with a reason, audited).
	PermReversePayments Permission = "payments.reverse"
)

// AllPermissions lists every permission (used by the RBAC matrix test).
var AllPermissions = []Permission{PermRead, PermManageApps, PermManageUsers, PermResolvePayments, PermReversePayments}

// rolePermissions is the single source of truth for what each role may do.
// finance and approver gain money permissions (refund maker / approver,
// statement import) in later phases.
var rolePermissions = map[store.AdminRole]map[Permission]bool{
	store.AdminRoleViewer:   {PermRead: true},
	store.AdminRoleFinance:  {PermRead: true, PermResolvePayments: true},
	store.AdminRoleApprover: {PermRead: true},
	store.AdminRoleAdmin: {
		PermRead: true, PermManageApps: true, PermManageUsers: true, PermResolvePayments: true, PermReversePayments: true,
	},
}

// Allows reports whether role grants perm. Unknown roles get nothing.
func Allows(role store.AdminRole, perm Permission) bool { return rolePermissions[role][perm] }

// Permissions lists what role may do, for the UI to hide what it cannot.
func Permissions(role store.AdminRole) []Permission {
	var out []Permission
	for _, p := range AllPermissions {
		if Allows(role, p) {
			out = append(out, p)
		}
	}
	return out
}
