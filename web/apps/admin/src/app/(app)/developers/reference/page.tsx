import { buttonClass, cx } from "@talksasa/ui";
import type { Metadata } from "next";
import { Fragment, type ReactNode } from "react";

import { Markdown } from "@/components/Markdown";
import { PageHeader } from "@/components/PageHeader";
import { type Field, type Operation, REFERENCE } from "@/content/developer-docs";

export const metadata: Metadata = { title: "API reference · Developers" };

const SCHEMAS = new Set(REFERENCE.schemas.map((s) => s.name));

const METHOD_TONE: Record<string, string> = {
  GET: "bg-info-soft text-info",
  POST: "bg-ok-soft text-ok",
  PUT: "bg-warn-soft text-warn",
  PATCH: "bg-warn-soft text-warn",
  DELETE: "bg-bad-soft text-bad",
};

function Method({ m }: { m: string }) {
  return (
    <span className={cx("inline-flex h-6 min-w-14 items-center justify-center rounded px-1.5 font-mono text-[0.75rem] font-semibold", METHOD_TONE[m] ?? "bg-wash text-ink-2")}>
      {m}
    </span>
  );
}

/** A type string with schema names linked to their definitions. */
function TypeName({ type }: { type: string }) {
  const parts = type.split(/([A-Za-z_][A-Za-z0-9_]*)/);
  return (
    <code className="font-mono text-[0.8125rem] break-words text-ink-2">
      {parts.map((p, i) =>
        SCHEMAS.has(p) ? (
          <a key={i} href={`#schema-${p}`} className="text-violet underline decoration-violet/30 underline-offset-2 hover:decoration-violet">
            {p}
          </a>
        ) : (
          <Fragment key={i}>{p}</Fragment>
        ),
      )}
    </code>
  );
}

function Prose({ text, cell = false }: { text: string; cell?: boolean }) {
  return text ? <Markdown source={text} className={cx("tsp-prose text-sm", cell && "tsp-prose-cell")} /> : null;
}

function FieldTable({ fields, label }: { fields: Field[]; label: string }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[36rem] table-fixed text-sm" aria-label={label}>
        <colgroup>
          <col className="w-[32%]" />
          <col className="w-[26%]" />
          <col />
        </colgroup>
        <thead>
          <tr className="border-b border-line text-left text-[0.8125rem] text-muted">
            <th className="py-2 pr-4 font-medium">Field</th>
            <th className="py-2 pr-4 font-medium">Type</th>
            <th className="py-2 font-medium">Notes</th>
          </tr>
        </thead>
        <tbody>
          {fields.map((f) => (
            <tr key={f.name} className="border-b border-line last:border-0 align-top">
              <td className="py-2.5 pr-4 break-words">
                <code className="font-mono text-[0.8125rem] font-medium text-ink">{f.name}</code>
                {f.required ? <span className="ml-1.5 text-[0.75rem] text-bad">required</span> : null}
              </td>
              <td className="py-2.5 pr-4">
                <TypeName type={f.type} />
              </td>
              <td className="py-2.5 text-ink-2">
                {f.description ? <Prose text={f.description} cell /> : null}
                {f.constraints.length ? <p className="mt-0.5 text-[0.8125rem] text-muted">{f.constraints.join(", ")}</p> : null}
                {!f.description && !f.constraints.length ? <span className="text-faint">—</span> : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Sub({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="mt-5">
      <h4 className="mb-1.5 text-[0.75rem] font-medium tracking-wide text-faint uppercase">{title}</h4>
      {children}
    </div>
  );
}

function OperationCard({ op, base, anchor }: { op: Operation; base: string; anchor?: string }) {
  return (
    <section id={anchor ?? op.id} className="scroll-mt-6 rounded-lg border border-line bg-surface">
      <header className="flex flex-wrap items-center gap-3 border-b border-line px-5 py-3.5">
        <Method m={op.method} />
        <code className="font-mono text-[0.9rem] font-medium break-all text-ink">{base + op.path}</code>
      </header>
      <div className="p-5">
        <h3 className="font-display text-[1rem] font-semibold text-ink">{op.summary}</h3>
        {op.description ? (
          <div className="mt-2">
            <Prose text={op.description} />
          </div>
        ) : null}
        {op.params.length ? (
          <Sub title="Parameters">
            <FieldTable
              label="Parameters"
              fields={op.params.map((p) => ({ name: p.name, type: `${p.type} · ${p.in}`, required: p.required, description: p.description, constraints: [] }))}
            />
          </Sub>
        ) : null}
        {op.body ? (
          <Sub title="Request body">
            <p className="mb-1 text-sm">
              <TypeName type={op.body.type} />
            </p>
            {op.body.fields.length ? <FieldTable label="Request body" fields={op.body.fields} /> : null}
          </Sub>
        ) : null}
        <Sub title="Responses">
          <ul className="divide-y divide-line text-sm">
            {op.responses.map((r) => (
              <li key={r.status} className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 py-2">
                <code className={cx("w-16 shrink-0 font-mono font-medium", r.status.startsWith("2") ? "text-ok" : "text-bad")}>
                  {r.status === "default" ? "4xx/5xx" : r.status}
                </code>
                <span className="text-ink-2">{r.status === "default" ? "Problem (application/problem+json)" : r.description}</span>
                {r.schema && r.status !== "default" ? <TypeName type={r.schema} /> : null}
              </li>
            ))}
          </ul>
        </Sub>
      </div>
    </section>
  );
}

export default function ApiReferencePage() {
  const r = REFERENCE;
  return (
    <>
      <PageHeader
        title="API reference"
        lead={`${r.title}, version ${r.version}. Generated from api/product.openapi.yaml; if a guide disagrees, this wins.`}
        actions={
          <a href="/developers/openapi.yaml" download className={buttonClass("secondary", "sm")}>
            Download OpenAPI
          </a>
        }
      />

      <div className="max-w-[56rem] space-y-10">
        <section className="rounded-lg border border-line bg-surface p-5">
          <Prose text={r.description} />
        </section>

        <section aria-labelledby="endpoints">
          <h2 id="endpoints" className="mb-3 font-display text-[1.2rem] font-semibold text-ink">
            Endpoints
          </h2>
          <ul className="divide-y divide-line rounded-lg border border-line bg-surface">
            {r.operations.map((op) => (
              <li key={op.id}>
                <a href={`#${op.id}`} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 hover:bg-wash">
                  <Method m={op.method} />
                  <code className="font-mono text-[0.8125rem] text-ink">{r.basePath + op.path}</code>
                  <span className="text-sm text-muted">{op.summary}</span>
                </a>
              </li>
            ))}
          </ul>
        </section>

        <div className="space-y-5">
          {r.operations.map((op) => (
            <OperationCard key={op.id} op={op} base={r.basePath} />
          ))}
        </div>

        {r.webhooks.length ? (
          <section aria-labelledby="webhooks-heading" className="space-y-5">
            <h2 id="webhooks-heading" className="font-display text-[1.2rem] font-semibold text-ink">
              Webhooks
            </h2>
            <p className="text-sm text-muted">Talksasa Pay calls your product&apos;s webhook URL. Your endpoint receives:</p>
            {r.webhooks.map((op, i) => (
              <OperationCard key={op.id} op={{ ...op, path: "your webhook URL" }} base="" anchor={i === 0 ? "webhook-payload" : op.id} />
            ))}
          </section>
        ) : null}

        <section aria-labelledby="schemas" className="space-y-5">
          <h2 id="schemas" className="font-display text-[1.2rem] font-semibold text-ink">
            Schemas
          </h2>
          {r.schemas.map((s) => (
            <section key={s.name} id={`schema-${s.name}`} className="scroll-mt-6 rounded-lg border border-line bg-surface p-5">
              <h3 className="flex flex-wrap items-baseline gap-3">
                <code className="font-mono text-[0.95rem] font-semibold text-ink">{s.name}</code>
                {s.type !== "object" ? <TypeName type={s.type} /> : null}
              </h3>
              {s.description ? (
                <div className="mt-2">
                  <Prose text={s.description} />
                </div>
              ) : null}
              {s.fields.length ? (
                <div className="mt-3">
                  <FieldTable label={s.name} fields={s.fields} />
                </div>
              ) : null}
            </section>
          ))}
        </section>
      </div>
    </>
  );
}
