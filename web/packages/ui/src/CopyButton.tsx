"use client";

import { useState } from "react";

import { cx } from "./cx";

/** Copies `value` to the clipboard and confirms in place. */
export function CopyButton({ value, label = "Copy", className }: { value: string; label?: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className={cx(
        "inline-flex h-8 items-center rounded-md border border-line-strong bg-surface px-3 text-[0.8125rem] font-medium text-ink-2 hover:bg-wash",
        className,
      )}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(value);
          setCopied(true);
          setTimeout(() => setCopied(false), 2000);
        } catch {
          setCopied(false);
        }
      }}
      aria-live="polite"
    >
      {copied ? "Copied" : label}
    </button>
  );
}
