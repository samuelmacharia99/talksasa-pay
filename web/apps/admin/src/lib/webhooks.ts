// Pure helpers for webhook deliveries and API key IP allow-lists. No
// server-only imports: client components use these too.

export type DeliveryStatus = "pending" | "delivered" | "failed" | "skipped";

export const DELIVERY_STATUS: Record<DeliveryStatus, { label: string; help: string }> = {
  pending: { label: "Pending", help: "Waiting for its first attempt or for the next retry." },
  delivered: { label: "Delivered", help: "The app answered with a 2xx status." },
  failed: { label: "Failed", help: "Every retry failed. The app did not get this event by webhook." },
  skipped: { label: "Skipped", help: "Not sent, usually because the app had no webhook URL at the time." },
};

export function deliveryStatusLabel(status: string): string {
  return DELIVERY_STATUS[status as DeliveryStatus]?.label ?? status.replace(/_/g, " ");
}

/** Only deliveries that have stopped without success can be queued again. */
export function canRedeliver(status: string): boolean {
  return status === "failed" || status === "skipped";
}

/**
 * Where the event's subject lives in the panel. A payment prompt has no page
 * of its own: it links to its charge when the event body names it.
 */
export function subjectHref(subjectType: string, subjectId: string, data?: unknown): string | undefined {
  switch (subjectType) {
    case "charge":
      return `/charges/${subjectId}`;
    case "transaction":
      return `/transactions/${subjectId}`;
    case "stk_push": {
      const chargeId = data && typeof data === "object" ? (data as Record<string, unknown>).charge_id : undefined;
      return typeof chargeId === "string" && /^[0-9a-f-]{36}$/i.test(chargeId) ? `/charges/${chargeId}` : undefined;
    }
    default:
      return undefined;
  }
}

export const SUBJECT_LABEL: Record<string, string> = {
  charge: "Charge",
  transaction: "Transaction",
  stk_push: "Payment prompt",
};

/** HTTP status code of an attempt as a tone for display. */
export function attemptTone(statusCode: number | null): "ok" | "bad" {
  return statusCode !== null && statusCode >= 200 && statusCode < 300 ? "ok" : "bad";
}

export const MAX_ALLOWED_IPS = 50;

/**
 * Splits the allow-list textarea into entries: one per line (commas and
 * spaces also separate), blanks dropped, duplicates removed, order kept.
 * An empty list means "any address" and is sent as null.
 */
export function splitAllowedIPs(text: string): string[] | null {
  const seen = new Set<string>();
  for (const raw of text.split(/[\s,]+/)) {
    const v = raw.trim();
    if (v) seen.add(v);
  }
  return seen.size === 0 ? null : [...seen];
}

function isIPv4(s: string): boolean {
  const parts = s.split(".");
  return parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
}

function isIPv6(s: string): boolean {
  // Light check only: the Go service parses properly and is authoritative.
  if (!s.includes(":") || !/^[0-9a-fA-F:.]+$/.test(s)) return false;
  const doubleColons = s.split("::").length - 1;
  return doubleColons <= 1 && s.length <= 45;
}

/** Why an allow-list entry looks wrong, or undefined when it looks fine. */
export function checkAllowedIP(entry: string): string | undefined {
  const [addr = "", bits, extra] = entry.split("/");
  if (extra !== undefined) return `${entry}: use one address or range per line.`;
  const v4 = isIPv4(addr);
  const v6 = !v4 && isIPv6(addr);
  if (!v4 && !v6) return `${entry} is not an IP address or CIDR range.`;
  if (bits !== undefined) {
    const max = v4 ? 32 : 128;
    if (!/^\d{1,3}$/.test(bits) || Number(bits) < 1 || Number(bits) > max) {
      return `${entry}: the range size must be between /1 and /${max}.`;
    }
  }
  return undefined;
}

/** Client-side check before saving. Returns the problems found (empty = fine). */
export function validateAllowedIPs(text: string): string[] {
  const list = splitAllowedIPs(text);
  if (!list) return [];
  const problems: string[] = [];
  if (list.length > MAX_ALLOWED_IPS) problems.push(`At most ${MAX_ALLOWED_IPS} entries. You have ${list.length}.`);
  for (const e of list) {
    const p = checkAllowedIP(e);
    if (p) problems.push(p);
  }
  return problems;
}

/** Pretty-prints a JSON value for display; never throws. */
export function prettyJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2) ?? "";
  } catch {
    return String(v);
  }
}
