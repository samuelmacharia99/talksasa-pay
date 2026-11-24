package admin

import "github.com/talksasa/talksasa-pay/internal/store"

// Permission is a capability checked per admin API route.
type Permission string

const (
	// PermRead allows viewing every screen: products, payers, requests,
	// transactions, suspense, audit log, dashboard.
	PermRead Permission = "read"
	// PermManageProducts allows creating products, changing webhooks,
	// rotating webhook secrets and issuing/revoking API keys.
	PermManageProducts Permission = "products.manage"
	// PermManageUsers allows inviting, disabling and re-roling staff.
	PermManageUsers Permission = "users.manage"
	// PermResolvePayments allows confirming (with the M-Pesa receipt from
	// the portal) or failing an M-Pesa payment in needs_review. Confirming
	// records money, so it is limited to finance and admin (user decision
	// 2026-11-24); every resolution is audited with a reason.
	PermResolvePayments Permission = "payments.resolve"
)

// AllPermissions lists every permission (used by the RBAC matrix test).
var AllPermissions = []Permission{PermRead, PermManageProducts, PermManageUsers, PermResolvePayments}

// rolePermissions is the single source of truth for what each role may do.
// finance and approver gain money permissions (refund maker / approver,
// statement import) in later phases.
var rolePermissions = map[store.AdminRole]map[Permission]bool{
	store.AdminRoleViewer:   {PermRead: true},
	store.AdminRoleFinance:  {PermRead: true, PermResolvePayments: true},
	store.AdminRoleApprover: {PermRead: true},
	store.AdminRoleAdmin:    {PermRead: true, PermManageProducts: true, PermManageUsers: true, PermResolvePayments: true},
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
