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

export const BATCH_KIND: Record<string, string> = {
  payment: "Payment",
  credit_application: "Paid from credit",
  reversal: "Reversal",
};

export const ROLE: Record<string, { label: string; can: string }> = {
  viewer: { label: "Viewer", can: "Sees everything, changes nothing." },
  finance: { label: "Finance", can: "Viewer access now; prepares refunds once refunds launch." },
  approver: { label: "Approver", can: "Viewer access now; approves refunds once refunds launch." },
  admin: { label: "Admin", can: "Manages products, API keys and staff." },
};

/** Turns "cli:jane" / "admin:<uuid>" / "system:worker" into something readable. */
export function actorLabel(actor: string, names?: Map<string, string>): string {
  const [type, id = ""] = actor.split(":", 2) as [string, string?];
  switch (type) {
    case "admin":
      return names?.get(id) ?? "Staff member";
    case "cli":
      return `${id} (command line)`;
    case "product":
      return "Product API";
    case "system":
      return `System (${id})`;
    default:
      return actor;
  }
}
