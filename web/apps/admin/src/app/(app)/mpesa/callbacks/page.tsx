import { DateTime, Empty, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { FilterBar, FilterField, filterInput } from "@/components/FilterBar";
import { PageHeader, Panel } from "@/components/PageHeader";
import { Pager } from "@/components/Pager";
import { ToneLabel } from "@/components/ToneLabel";
import { load } from "@/lib/api";
import { CALLBACK_KIND, callbackState } from "@/lib/mpesa";
import { anySet, pick } from "@/lib/params";

export const metadata: Metadata = { title: "M-Pesa callbacks" };

const KEYS = ["unprocessed", "cursor"] as const;

export default async function MpesaCallbacks({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const p = pick(await searchParams, KEYS);
  const unprocessed = p.unprocessed === "true" ? "true" : undefined;
  const page = await load((c) =>
    c.GET("/mpesa/callbacks", { params: { query: { unprocessed: unprocessed ? true : undefined, cursor: p.cursor, limit: 50 } } }),
  );
  return (
    <>
      <PageHeader
        title="M-Pesa callbacks"
        lead="Every message Safaricom sent us about payment prompts and paybill payments, newest first. Each one is stored before it is applied, so nothing Safaricom sends is lost. One that stays unprocessed needs a look."
      />
      <FilterBar action="/mpesa/callbacks" active={anySet({ unprocessed })}>
        <FilterField label="Show">
          <select name="unprocessed" defaultValue={unprocessed ?? ""} className={filterInput}>
            <option value="">All callbacks</option>
            <option value="true">Not processed yet</option>
          </select>
        </FilterField>
      </FilterBar>
      <Panel flush>
        {page.items.length === 0 ? (
          <Empty title={unprocessed ? "Everything is processed" : "No callbacks yet"}>
            {unprocessed
              ? "Every message from Safaricom has been applied."
              : "Callbacks appear here as soon as Safaricom reports a payment prompt result or a paybill payment."}
          </Empty>
        ) : (
          <Table label="M-Pesa callbacks">
            <thead>
              <tr>
                <Th>Received</Th>
                <Th>Kind</Th>
                <Th>Status</Th>
                <Th align="right">Attempts</Th>
                <Th>Error</Th>
                <Th>Payment</Th>
                <Th>From</Th>
              </tr>
            </thead>
            <tbody>
              {page.items.map((cb) => {
                const s = callbackState(cb);
                return (
                  <tr key={cb.id} className="hover:bg-wash/60">
                    <Td>
                      <DateTime value={cb.received_at} />
                    </Td>
                    <Td className="text-ink-2">{CALLBACK_KIND[cb.kind] ?? cb.kind}</Td>
                    <Td>
                      <ToneLabel label={s.label} tone={s.tone} />
                      {cb.processed_at ? (
                        <span className="block text-[0.75rem] text-muted">
                          <DateTime value={cb.processed_at} />
                        </span>
                      ) : null}
                    </Td>
                    <Td align="right" className="num">
                      {cb.attempts}
                    </Td>
                    <Td className="max-w-80">
                      {cb.error ? (
                        <span className="block truncate text-[0.8125rem] text-bad" title={cb.error}>
                          {cb.error}
                        </span>
                      ) : (
                        <span className="text-faint">—</span>
                      )}
                    </Td>
                    <Td>
                      {cb.transaction_id ? (
                        <Link href={`/transactions/${cb.transaction_id}`} className="text-violet hover:underline">
                          View payment
                        </Link>
                      ) : (
                        <span className="text-faint">—</span>
                      )}
                    </Td>
                    <Td className="font-mono text-[0.75rem] text-muted">{cb.remote_ip ?? "—"}</Td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
        )}
        <Pager basePath="/mpesa/callbacks" params={{ unprocessed, cursor: p.cursor }} nextCursor={page.next_cursor} />
      </Panel>
    </>
  );
}
