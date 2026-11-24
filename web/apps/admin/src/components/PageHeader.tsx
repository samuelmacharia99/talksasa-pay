import type { ReactNode } from "react";

/** Page title, an optional one-line explanation, and actions on the right. */
export function PageHeader({ title, lead, actions, back }: { title: ReactNode; lead?: ReactNode; actions?: ReactNode; back?: ReactNode }) {
  return (
    <header className="mb-8">
      {back ? <div className="mb-3 text-sm">{back}</div> : null}
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="min-w-0">
          <h1 className="font-display text-[1.75rem] leading-tight font-semibold tracking-[-0.02em] text-ink">{title}</h1>
          {lead ? <p className="mt-1.5 max-w-2xl text-[0.9375rem] text-muted">{lead}</p> : null}
        </div>
        {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
      </div>
    </header>
  );
}

/** A plain white panel; used for tables and detail blocks. */
export function Panel({ title, actions, children, flush = false }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; flush?: boolean }) {
  return (
    <section className="rounded-lg border border-line bg-surface">
      {title ? (
        <div className="flex items-center justify-between gap-4 border-b border-line px-5 py-3.5">
          <h2 className="font-display text-[0.95rem] font-semibold">{title}</h2>
          {actions}
        </div>
      ) : null}
      <div className={flush ? "" : "p-5"}>{children}</div>
    </section>
  );
}

/** Label/value pairs for detail pages. */
export function Facts({ items }: { items: Array<[string, ReactNode]> }) {
  return (
    <dl className="grid gap-x-8 gap-y-4 sm:grid-cols-2 lg:grid-cols-3">
      {items.map(([k, v]) => (
        <div key={k} className="min-w-0">
          <dt className="text-[0.8125rem] text-muted">{k}</dt>
          <dd className="mt-0.5 text-sm break-words text-ink">{v}</dd>
        </div>
      ))}
    </dl>
  );
}
