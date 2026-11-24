import { DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { ProductSelect } from "@/components/ProductSelect";
import { load, type QueryOf } from "@/lib/api";
import { anySet, pick } from "@/lib/params";
import { DELIVERY_STATUS, SUBJECT_LABEL, subjectHref } from "@/lib/webhooks";

export const metadata: Metadata = { title: "Webhooks" };

const KEYS = ["product_id", "status", "cursor"] as const;

export default async function Webhooks({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, KEYS);
  const [page, products] = await Promise.all([
    load((c) => c.GET("/webhook-deliveries", { params: { query: { ...p, limit: 50 } as QueryOf<"/webhook-deliveries"> } })),
    load((c) => c.GET("/products")),
  ]);
  return (
    <>
      <PageHeader
        title="Webhooks"
        lead="Every event we sent, or tried to send, to a product's webhook URL, newest first. Products can also read every event from their events feed, so a failed webhook does not mean lost data."
      />
      <FilterBar action="/webhooks" active={anySet(p)}>
        <FilterField label="Product">
          <ProductSelect products={products.items} value={p.product_id} />
        </FilterField>
        <FilterField label="Status">
          <select name="status" defaultValue={p.status ?? ""} className={filterInput}>
            <option value="">Any</option>
            {Object.entries(DELIVERY_STATUS).map(([value, s]) => (
              <option key={value} value={value}>
                {s.label}
              </option>
            ))}
          </select>
        </FilterField>
      </FilterBar>
      <Panel flush>
        {page.items.length === 0 ? (
          <Empty title={anySet(p) ? "No deliveries match" : "No webhooks sent yet"}>
            {anySet(p) ? "Try another status or clear the filters." : "Deliveries appear here as soon as a product's bills or payments change."}
          </Empty>
        ) : (
          <Table label="Webhook deliveries">
            <thead>
              <tr>
                <Th>Created</Th>
                <Th>Product</Th>
                <Th>Event</Th>
                <Th>About</Th>
                <Th>Status</Th>
                <Th align="right">Attempts</Th>
                <Th>Last attempt</Th>
              </tr>
            </thead>
            <tbody>
              {page.items.map((d) => {
                const href = subjectHref(d.subject_type, d.subject_id);
                const subject = SUBJECT_LABEL[d.subject_type] ?? d.subject_type;
                return (
                  <tr key={d.event_id} className="hover:bg-wash/60">
                    <Td>
                      <Link href={`/webhooks/${d.event_id}`} className="text-ink hover:text-violet hover:underline">
                        <DateTime value={d.created_at} />
                      </Link>
                    </Td>
                    <Td className="text-ink-2">{d.account_prefix}</Td>
                    <Td className="font-mono text-[0.8125rem]">{d.event_type}</Td>
                    <Td>
                      {href ? (
                        <Link href={href} className="text-violet hover:underline">
                          {subject}
                        </Link>
                      ) : (
                        subject
                      )}
                    </Td>
                    <Td>
                      <Status value={d.status} />
                    </Td>
                    <Td align="right" className="num">
                      {d.attempts}
                    </Td>
                    <Td className="max-w-72">
                      <DateTime value={d.last_attempt_at} fallback="Not tried yet" />
                      {d.last_status_code !== null || d.last_error ? (
                        <span className="block truncate text-[0.75rem] text-muted" title={d.last_error ?? undefined}>
                          {d.last_status_code !== null ? `HTTP ${d.last_status_code}` : null}
                          {d.last_status_code !== null && d.last_error ? ", " : null}
                          {d.last_error}
                        </span>
                      ) : null}
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
        )}
        <Pager basePath="/webhooks" params={p} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
