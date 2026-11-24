import { cx, DateTime, Status } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { getMe, load } from "@/lib/api";
import { attemptTone, canRedeliver, DELIVERY_STATUS, prettyJSON, SUBJECT_LABEL, subjectHref } from "@/lib/webhooks";

import { RedeliverForm } from "../forms";

export const metadata: Metadata = { title: "Webhook delivery" };

export default async function WebhookDeliveryPage({ params }: { params: Promise<{ eventID: string }> }) {
  const { eventID } = await params;
  const [w, me] = await Promise.all([
    load((c) => c.GET("/webhook-deliveries/{eventID}", { params: { path: { eventID } } })),
    getMe(),
  ]);
  const d = w.delivery;
  const canManage = me.permissions.includes("products.manage");
  const href = subjectHref(d.subject_type, d.subject_id);
  const subject = SUBJECT_LABEL[d.subject_type] ?? d.subject_type;
  // Newest attempt first: that is the one staff usually need.
  const attempts = [...w.attempts].sort((a, b) => b.attempt - a.attempt);

  return (
    <>
      <PageHeader
        back={<Link href="/webhooks" className="text-violet hover:underline">Webhooks</Link>}
        title={<span className="font-mono text-[1.5rem]">{d.event_type}</span>}
        lead={DELIVERY_STATUS[d.status]?.help}
        actions={<Status value={d.status} className="text-sm" />}
      />
      <div className="grid gap-6">
        <Panel title="Details">
          <Facts
            items={[
              [
                "Product",
                <Link key="p" href={`/products/${d.product_id}`} className="text-violet hover:underline">
                  {d.account_prefix}
                </Link>,
              ],
              [
                "About",
                href ? (
                  <Link key="s" href={href} className="text-violet hover:underline">
                    {subject}
                  </Link>
                ) : (
                  subject
                ),
              ],
              ["Event id", <span key="e" className="font-mono text-[0.8125rem]">{d.event_id}</span>],
              ["Created", <DateTime key="c" value={d.created_at} />],
              ["Attempts", String(d.attempts)],
              ["Last attempt", <DateTime key="l" value={d.last_attempt_at} fallback="Not tried yet" />],
              ["Delivered", <DateTime key="d" value={d.delivered_at} fallback="Not yet" />],
              ...(d.last_error ? ([["Last error", d.last_error]] as Array<[string, string]>) : []),
            ]}
          />
        </Panel>

        {canManage && canRedeliver(d.status) ? (
          <Panel title="Redeliver">
            <RedeliverForm eventID={d.event_id} productLabel={d.account_prefix} />
          </Panel>
        ) : null}

        <Panel title="Attempts" flush>
          {attempts.length === 0 ? (
            <p className="p-5 text-sm text-muted">
              {d.status === "skipped" ? "No attempt was made." : "No attempt yet. The first one runs within moments of the event."}
            </p>
          ) : (
            <ol className="divide-y divide-line">
              {attempts.map((a) => {
                const tone = attemptTone(a.status_code);
                return (
                  <li key={a.attempt} className="px-5 py-4">
                    <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
                      <span className="font-medium text-ink">Attempt {a.attempt}</span>
                      <DateTime value={a.attempted_at} />
                      <span className={cx("inline-flex items-center gap-1.5 font-medium", tone === "ok" ? "text-ok" : "text-bad")}>
                        <span aria-hidden className={cx("size-1.5 rounded-full", tone === "ok" ? "bg-ok" : "bg-bad")} />
                        {a.status_code !== null ? `HTTP ${a.status_code}` : "No response"}
                      </span>
                      <span className="num text-muted">{a.latency_ms} ms</span>
                      <span className="font-mono text-[0.8125rem] text-muted">{a.url_host}</span>
                    </div>
                    {a.error ? <p className="mt-1.5 text-[0.8125rem] text-bad">{a.error}</p> : null}
                    {a.response_body ? (
                      <details className="mt-2">
                        <summary className="cursor-pointer text-[0.8125rem] text-muted hover:text-ink">Response (first 1 KB)</summary>
                        <pre className="mt-2 max-h-64 overflow-auto rounded-md bg-wash p-3 font-mono text-[0.75rem] leading-relaxed whitespace-pre-wrap text-ink-2">
                          {a.response_body}
                        </pre>
                      </details>
                    ) : null}
                  </li>
                );
              })}
            </ol>
          )}
        </Panel>

        <Panel title="Event body">
          <p className="mb-3 text-[0.8125rem] text-muted">
            The <span className="font-mono">data</span> the product receives, as it was when the event happened. Created{" "}
            <DateTime value={w.event.created_at} />.
          </p>
          <pre className="overflow-x-auto rounded-md bg-wash p-4 font-mono text-[0.75rem] leading-relaxed text-ink-2">{prettyJSON(w.event.data)}</pre>
        </Panel>
      </div>
    </>
  );
}
