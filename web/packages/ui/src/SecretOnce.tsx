"use client";

import { useState } from "react";

import { CopyButton } from "./CopyButton";

/**
 * Shows a secret that the server will never return again (API key, webhook
 * signing secret, setup link). The value lives only in this component's
 * memory; dismissing it removes it from the page.
 */
export function SecretOnce({ title, value, help }: { title: string; value: string; help: string }) {
  const [open, setOpen] = useState(true);
  if (!open) {
    return <p className="text-sm text-muted">Secret hidden. It cannot be shown again; issue a new one if it was lost.</p>;
  }
  return (
    <div role="status" className="rounded-lg border border-warn/40 bg-warn-soft p-4">
      <p className="font-medium text-ink">{title}</p>
      <p className="mt-1 text-sm text-ink-2">{help}</p>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <code className="min-w-0 flex-1 rounded-md border border-line bg-surface px-3 py-2 font-mono text-[0.8125rem] break-all text-ink">
          {value}
        </code>
        <CopyButton value={value} />
      </div>
      <button type="button" onClick={() => setOpen(false)} className="mt-3 text-sm font-medium text-violet hover:underline">
        I have stored it safely
      </button>
    </div>
  );
}
