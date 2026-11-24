import { cx } from "./cx";

type Tone = "ok" | "warn" | "bad" | "info" | "neutral" | "violet";

const STATUSES: Record<string, { label: string; tone: Tone }> = {
  // payment requests
  open: { label: "Open", tone: "info" },
  partially_paid: { label: "Part paid", tone: "warn" },
  paid: { label: "Paid", tone: "ok" },
  cancelled: { label: "Cancelled", tone: "neutral" },
  expired: { label: "Expired", tone: "neutral" },
  // transactions
  pending: { label: "Pending", tone: "info" },
  succeeded: { label: "Succeeded", tone: "ok" },
  failed: { label: "Failed", tone: "bad" },
  needs_review: { label: "Needs review", tone: "bad" },
  // products, payers, staff
  active: { label: "Active", tone: "ok" },
  suspended: { label: "Suspended", tone: "bad" },
  closed: { label: "Closed", tone: "neutral" },
  invited: { label: "Invited", tone: "violet" },
  disabled: { label: "Disabled", tone: "neutral" },
  revoked: { label: "Revoked", tone: "neutral" },
  expired_key: { label: "Expired", tone: "neutral" },
  // webhook deliveries (pending and failed are shared with transactions)
  delivered: { label: "Delivered", tone: "ok" },
  skipped: { label: "Skipped", tone: "neutral" },
};

const dot: Record<Tone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  bad: "bg-bad",
  info: "bg-info",
  neutral: "bg-faint",
  violet: "bg-violet",
};

const text: Record<Tone, string> = {
  ok: "text-ok",
  warn: "text-warn",
  bad: "text-bad",
  info: "text-info",
  neutral: "text-muted",
  violet: "text-violet",
};

/** A status as a coloured dot and a word; never colour alone. */
export function Status({ value, className }: { value: string; className?: string }) {
  const s = STATUSES[value] ?? { label: value.replace(/_/g, " "), tone: "neutral" as Tone };
  return (
    <span className={cx("inline-flex items-center gap-1.5 text-[0.8125rem] font-medium", text[s.tone], className)}>
      <span aria-hidden className={cx("size-1.5 rounded-full", dot[s.tone])} />
      {s.label}
    </span>
  );
}
