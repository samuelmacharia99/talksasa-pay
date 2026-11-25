export const CHANNEL: Record<string, string> = {
  mpesa_stk: "M-Pesa prompt",
  mpesa_c2b: "M-Pesa paybill",
  mpesa_b2c: "M-Pesa payout",
  manual: "Manual",
};

export const KIND: Record<string, string> = {
  payment: "Payment",
  refund: "Refund",
  reversal: "Reversal",
  adjustment: "Adjustment",
};

export const ROLE: Record<string, { label: string; can: string }> = {
  viewer: { label: "Viewer", can: "Sees everything, changes nothing." },
  finance: { label: "Finance", can: "Resolves payments in review and assigns unmatched payments to apps." },
  approver: { label: "Approver", can: "Viewer access now; approves refunds once refunds launch." },
  admin: { label: "Admin", can: "Everything finance can, plus reversals, apps, API keys and staff." },
};

/** Charge statuses with what each means for staff. */
export const CHARGE_STATUS: Record<string, { label: string; help: string }> = {
  pending: { label: "Pending", help: "Waiting for the customer to pay." },
  needs_review: { label: "Needs review", help: "Safaricom said it was paid but sent no receipt. Staff confirm it on the transaction." },
  succeeded: { label: "Succeeded", help: "Paid with an M-Pesa receipt, for exactly the charge amount." },
  expired: { label: "Expired", help: "Not paid in time. Money that still arrives will pay it." },
  cancelled: { label: "Cancelled", help: "The app cancelled it. Money that still arrives will pay it." },
  reversed: { label: "Reversed", help: "Paid, then reversed by staff. The app was told." },
};

/** Turns "cli:jane" / "admin:<uuid>" / "system:worker" into something readable. */
export function actorLabel(actor: string, names?: Map<string, string>): string {
  const [type, id = ""] = actor.split(":", 2) as [string, string?];
  switch (type) {
    case "admin":
      return names?.get(id) ?? "Staff member";
    case "cli":
      return `${id} (command line)`;
    case "app":
      return "App API";
    case "system":
      return `System (${id})`;
    default:
      return actor;
  }
}

/** Where an audit entry's subject lives in the panel, if it has a page. */
export function entityHref(type: string, id: string | null): string | undefined {
  if (!id) return undefined;
  switch (type) {
    case "app":
      return `/apps/${id}`;
    case "charge":
      return `/charges/${id}`;
    case "transaction":
      return `/transactions/${id}`;
    case "webhook_delivery":
      return `/webhooks/${id}`;
    default:
      return undefined;
  }
}
