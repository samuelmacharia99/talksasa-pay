import { cx } from "@talksasa/ui";

import type { Tone } from "@/lib/mpesa";

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

/** A coloured dot and a word, like Status, for labels Status does not know (prompts, callbacks). */
export function ToneLabel({ label, tone, className }: { label: string; tone: Tone; className?: string }) {
  return (
    <span className={cx("inline-flex items-center gap-1.5 text-[0.8125rem] font-medium", text[tone], className)}>
      <span aria-hidden className={cx("size-1.5 rounded-full", dot[tone])} />
      {label}
    </span>
  );
}
