/**
 * Charge detail helpers: the status timeline and what each transaction
 * naming the charge is. Pure, so they are unit tested.
 */

import type { Tone } from "./mpesa";

export interface TimelineCharge {
  status: string;
  created_at: string;
  succeeded_at: string | null;
  closed_at: string | null;
  transaction_id: string | null;
  stk_pushes: Array<{ id: string; status: string; created_at: string; completed_at: string | null; result_desc: string | null }>;
  transactions: Array<TimelineTxn>;
}

export interface TimelineTxn {
  id: string;
  kind: string;
  direction: string;
  status: string;
  amount: number;
  receipt: string | null;
  created_at: string;
  completed_at: string | null;
}

export interface TimelineEntry {
  at: string;
  label: string;
  detail?: string;
  tone: Tone;
  href?: string;
}

/**
 * What happened to a charge, oldest first: created, each prompt sent and its
 * outcome, paid, duplicates, needs review, expired or cancelled, reversed.
 */
export function chargeTimeline(c: TimelineCharge): TimelineEntry[] {
  const out: TimelineEntry[] = [{ at: c.created_at, label: "Created", tone: "info" }];
  for (const p of c.stk_pushes) {
    out.push({ at: p.created_at, label: "Payment prompt sent", tone: "violet" });
    if (p.status === "failed" && p.completed_at) {
      out.push({ at: p.completed_at, label: "Prompt failed", detail: p.result_desc ?? undefined, tone: "bad" });
    }
    if (p.status === "unknown") {
      out.push({ at: p.completed_at ?? p.created_at, label: "Prompt got no answer", detail: "Safaricom said paid but sent no receipt", tone: "warn" });
    }
  }
  for (const t of c.transactions) {
    const role = txnRole(t, c.transaction_id);
    if (role.key === "paid") {
      out.push({ at: c.succeeded_at ?? t.completed_at ?? t.created_at, label: "Paid", detail: t.receipt ?? undefined, tone: "ok", href: `/transactions/${t.id}` });
    } else if (role.key === "duplicate") {
      out.push({ at: t.completed_at ?? t.created_at, label: "Paid again (duplicate)", detail: t.receipt ?? undefined, tone: "warn", href: `/transactions/${t.id}` });
    } else if (role.key === "reversal") {
      out.push({ at: t.created_at, label: "Reversed", detail: t.receipt ?? undefined, tone: "bad", href: `/transactions/${t.id}` });
    } else if (role.key === "review") {
      out.push({ at: t.created_at, label: "Needs review", detail: "Waiting for staff to confirm the receipt", tone: "bad", href: `/transactions/${t.id}` });
    }
  }
  if (c.succeeded_at && !c.transactions.some((t) => t.id === c.transaction_id)) {
    out.push({ at: c.succeeded_at, label: "Paid", tone: "ok" });
  }
  if (c.closed_at && (c.status === "expired" || c.status === "cancelled")) {
    out.push({ at: c.closed_at, label: c.status === "expired" ? "Expired" : "Cancelled by the app", tone: "neutral" });
  }
  return out.sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
}

export type TxnRoleKey = "paid" | "duplicate" | "reversal" | "review" | "pending" | "failed" | "other";

/** What a transaction naming this charge is, from the charge's point of view. */
export function txnRole(t: Pick<TimelineTxn, "id" | "kind" | "direction" | "status">, paidBy: string | null): { key: TxnRoleKey; label: string; tone: Tone } {
  if (t.kind === "reversal") return { key: "reversal", label: "Reversal", tone: "bad" };
  if (t.id === paidBy) return { key: "paid", label: "Paid this charge", tone: "ok" };
  if (t.direction === "in" && t.status === "succeeded") return { key: "duplicate", label: "Duplicate payment", tone: "warn" };
  if (t.status === "needs_review") return { key: "review", label: "Needs review", tone: "bad" };
  if (t.status === "pending") return { key: "pending", label: "In progress", tone: "info" };
  if (t.status === "failed" || t.status === "cancelled") return { key: "failed", label: "Not paid", tone: "neutral" };
  return { key: "other", label: t.kind, tone: "neutral" };
}

/** Subject ids whose webhook events belong to this charge. */
export function chargeSubjects(c: { id: string; stk_pushes: Array<{ id: string }>; transactions: Array<{ id: string }> }): Set<string> {
  return new Set([c.id, ...c.stk_pushes.map((p) => p.id), ...c.transactions.map((t) => t.id)]);
}

/** A checkout link from an event body, if it carries one (charge events do). */
export function checkoutURLFrom(data: unknown): string | undefined {
  if (!data || typeof data !== "object") return undefined;
  const d = data as Record<string, unknown>;
  const candidates = [d.checkout_url, (d.charge as Record<string, unknown> | undefined)?.checkout_url];
  for (const v of candidates) {
    if (typeof v === "string" && /^https?:\/\//.test(v)) return v;
  }
  return undefined;
}
