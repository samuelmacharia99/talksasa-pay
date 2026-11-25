/**
 * The overview's count tiles and "Needs attention" list, built from GET
 * /dashboard counts. Pure, so it is unit tested.
 */

import { formatKes } from "@talksasa/money";

import type { Tone } from "./mpesa";

export interface DashboardCounts {
  charges_pending: number;
  charges_succeeded_today: number;
  charges_succeeded_today_amount: number;
  charges_needs_review: number;
  transactions_needs_review: number;
  unmatched_count: number;
  unmatched_amount: number;
  pending_transactions: number;
  unknown_stk_pushes: number;
  unprocessed_callbacks: number;
  inactive_app_payments: number;
}

export interface Tile {
  href: string;
  label: string;
  value: string;
  detail: string;
  tone: Tone;
}

export interface AttentionItem {
  href: string;
  label: string;
  detail: string;
  urgent: boolean;
}

const plural = (n: number, one: string, many: string) => (n === 1 ? `1 ${one}` : `${n} ${many}`);

/** Today's date in Nairobi as YYYY-MM-DD, for "from" filters. */
export function nairobiToday(now = new Date()): string {
  return new Intl.DateTimeFormat("en-CA", { timeZone: "Africa/Nairobi", year: "numeric", month: "2-digit", day: "2-digit" }).format(now);
}

/** The six headline counts, each linking to the list that explains it. */
export function overviewTiles(c: DashboardCounts, today: string): Tile[] {
  const review = c.transactions_needs_review;
  return [
    {
      href: "/charges?status=pending",
      label: "Charges pending",
      value: String(c.charges_pending),
      detail: "Waiting for the customer to pay",
      tone: "info",
    },
    {
      href: `/charges?status=succeeded&from=${today}`,
      label: "Collected today",
      value: formatKes(c.charges_succeeded_today_amount),
      detail: `${plural(c.charges_succeeded_today, "charge", "charges")} paid since midnight`,
      tone: "ok",
    },
    {
      href: "/transactions?status=needs_review",
      label: "Needs review",
      value: String(review),
      detail: review ? "Safaricom said paid but sent no receipt" : "Nothing waiting on staff",
      tone: review ? "bad" : "neutral",
    },
    {
      href: "/unmatched",
      label: "Unmatched",
      value: String(c.unmatched_count),
      detail: c.unmatched_count ? `${formatKes(c.unmatched_amount)} to assign to an app` : "Every payment found its app",
      tone: c.unmatched_count ? "warn" : "neutral",
    },
    {
      href: "/mpesa/callbacks?unprocessed=true",
      label: "Callback backlog",
      value: String(c.unprocessed_callbacks),
      detail: c.unprocessed_callbacks ? "Safaricom messages not yet applied" : "Every callback applied",
      tone: c.unprocessed_callbacks ? "bad" : "neutral",
    },
    {
      href: "/apps",
      label: "Inactive-app payments",
      value: String(c.inactive_app_payments),
      detail: c.inactive_app_payments ? "Paid to a suspended app, last 30 days" : "None in the last 30 days",
      tone: c.inactive_app_payments ? "warn" : "neutral",
    },
  ];
}

/** Things staff should act on now; only non-zero counts. */
export function attentionItems(c: DashboardCounts, failedWebhooks: { count: number; more: boolean } | null): AttentionItem[] {
  const out: AttentionItem[] = [];
  if (c.transactions_needs_review > 0) {
    out.push({
      href: "/transactions?status=needs_review",
      label: plural(c.transactions_needs_review, "payment needs review", "payments need review"),
      detail: "Find each one on the M-Pesa portal, then confirm it with its receipt or mark it not paid",
      urgent: true,
    });
  }
  if (c.unprocessed_callbacks > 0) {
    out.push({
      href: "/mpesa/callbacks?unprocessed=true",
      label: plural(c.unprocessed_callbacks, "M-Pesa callback not processed", "M-Pesa callbacks not processed"),
      detail: "Messages from Safaricom are stored but not yet applied; check the error column",
      urgent: true,
    });
  }
  if (failedWebhooks && failedWebhooks.count > 0) {
    const n = failedWebhooks.more ? `${failedWebhooks.count}+` : String(failedWebhooks.count);
    out.push({
      href: "/webhooks?status=failed",
      label: failedWebhooks.count === 1 && !failedWebhooks.more ? "1 webhook failed" : `${n} webhooks failed`,
      detail: "Apps did not receive these events by webhook; fix the cause, then redeliver",
      urgent: true,
    });
  }
  if (c.unmatched_count > 0) {
    out.push({
      href: "/unmatched",
      label: plural(c.unmatched_count, "unmatched payment", "unmatched payments"),
      detail: `${formatKes(c.unmatched_amount)} with an account prefix no app owns; assign each to its app`,
      urgent: false,
    });
  }
  if (c.inactive_app_payments > 0) {
    out.push({
      href: "/apps",
      label: plural(c.inactive_app_payments, "payment to a suspended app", "payments to suspended apps"),
      detail: "Recorded and forwarded as usual; tell the app's team customers are still paying",
      urgent: false,
    });
  }
  return out;
}
