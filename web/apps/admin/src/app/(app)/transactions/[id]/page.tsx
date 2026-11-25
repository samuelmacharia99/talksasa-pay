import { formatKes } from "@talksasa/money";
import { Amount, DateTime, Status } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";
import type { ReactNode } from "react";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { getMe, load } from "@/lib/api";
import { actorLabel, CHANNEL, KIND } from "@/lib/labels";
import { reverseConfirmWord } from "@/lib/mpesa";
import { prettyJSON } from "@/lib/webhooks";

import { AssignForm } from "./AssignForm";
import { ResolvePanel } from "./ResolvePanel";
import { ReversePanel } from "./ReversePanel";

export const metadata: Metadata = { title: "Transaction" };

const link = "text-violet hover:underline";

export default async function TransactionPage({
  params,
  searchParams,
}: {
  params: Promise<{ id: string }>;
  searchParams: Promise<{ assigned?: string }>;
}) {
  const { id } = await params;
  const { assigned } = await searchParams;
  const [t, me] = await Promise.all([load((c) => c.GET("/transactions/{id}", { params: { path: { id } } })), getMe()]);
  const out = t.direction === "out";
  const can = (p: (typeof me.permissions)[number]) => me.permissions.includes(p);
  const canResolve = t.status === "needs_review" && t.channel === "mpesa_stk" && can("payments.resolve");
  const succeededIn = t.status === "succeeded" && t.direction === "in" && t.kind === "payment";
  const unmatched = succeededIn && t.app_id === null;
  const canAssign = unmatched && can("payments.resolve");
  const canReverse = succeededIn && can("payments.reverse");
  const apps = canAssign ? (await load((c) => c.GET("/apps"))).items : [];

  const facts: Array<[string, ReactNode]> = [
    [
      "App",
      t.app_id ? (
        <Link key="a" href={`/apps/${t.app_id}`} className={link}>
          {t.app_name ?? t.account_prefix}
        </Link>
      ) : (
        <span key="a" className="text-warn">
          Unmatched
        </span>
      ),
    ],
    [
      "Charge",
      t.charge_id ? (
        <Link key="c" href={`/charges/${t.charge_id}`} className={link}>
          {t.charge_reference ?? "View charge"}
        </Link>
      ) : (
        "No charge"
      ),
    ],
    ["Account number", t.account_number ? <span className="font-mono text-[0.8125rem]">{t.account_number}</span> : "—"],
    ["Phone", t.phone ?? "—"],
    ["M-Pesa request id", t.provider_request_id ? <span className="font-mono text-[0.8125rem]">{t.provider_request_id}</span> : "—"],
    ["Recorded", <DateTime key="r" value={t.created_at} />],
    ["Completed", <DateTime key="d" value={t.completed_at} fallback="Not yet" />],
    ["M-Pesa time", <DateTime key="o" value={t.occurred_at} />],
  ];
  if (t.reason) facts.push(["Reason", t.reason]);
  if (t.created_by) facts.push(["Done by", actorLabel(t.created_by)]);
  if (t.failure_reason) facts.push(["Failure", `${t.failure_reason}${t.failure_code ? ` (code ${t.failure_code})` : ""}`]);
  if (t.reverses_transaction_id) {
    facts.push([
      "Reverses",
      <Link key="rv" href={`/transactions/${t.reverses_transaction_id}`} className={link}>
        the original payment
      </Link>,
    ]);
  }

  return (
    <>
      <PageHeader
        back={
          <Link href={unmatched ? "/unmatched" : "/transactions"} className={link}>
            {unmatched ? "Unmatched payments" : "Transactions"}
          </Link>
        }
        title={
          <>
            <Amount value={out ? -t.amount : t.amount} /> <span className="text-muted">{out ? "reversed" : "received"}</span>
          </>
        }
        lead={
          <>
            {KIND[t.kind] ?? t.kind} by {CHANNEL[t.channel] ?? t.channel}
            {t.receipt ? (
              <>
                , receipt <span className="font-mono text-ink">{t.receipt}</span>
              </>
            ) : null}
            .
          </>
        }
        actions={<Status value={t.status} className="text-sm" />}
      />
      {assigned && t.app_id ? (
        <p role="status" className="mb-4 text-sm text-ok">
          Assigned to {t.app_name ?? t.account_prefix}. The app has been told.
          {t.app_status && t.app_status !== "active" ? (
            <span className="block text-warn">That app is suspended: tell its team the money arrived.</span>
          ) : null}
        </p>
      ) : null}
      <div className="grid gap-6">
        {canResolve ? (
          <Panel title="Resolve this payment">
            <ResolvePanel id={t.id} amountLabel={formatKes(t.amount)} phone={t.phone} />
          </Panel>
        ) : t.status === "needs_review" ? (
          <Panel>
            <p className="text-sm text-ink-2">
              <span className="font-medium text-bad">Waiting for staff review.</span> Safaricom said this was paid but sent no receipt.
              Finance or an admin confirms it from the M-Pesa portal.
            </p>
          </Panel>
        ) : null}

        {unmatched ? (
          <Panel title="Assign to app">
            <p className="mb-4 max-w-2xl text-sm text-ink-2">
              The account number {t.account_number ? <span className="font-mono text-ink">“{t.account_number}”</span> : "typed"} starts with no
              app’s prefix, so the payment is held here. Check with the customer which app it was for, then assign it: the app gets a{" "}
              <span className="font-mono">payment.received</span> event.
            </p>
            {canAssign ? (
              <AssignForm id={t.id} apps={apps} amountLabel={formatKes(t.amount)} accountNumber={t.account_number} />
            ) : (
              <p className="text-sm text-muted">Finance or an admin assigns unmatched payments.</p>
            )}
          </Panel>
        ) : null}

        <Panel title="Details">
          <Facts items={facts} />
        </Panel>

        {canReverse ? (
          <Panel title="Reverse">
            <ReversePanel
              id={t.id}
              amountLabel={formatKes(t.amount)}
              receiptRequired={t.channel.startsWith("mpesa_")}
              expected={reverseConfirmWord(t.receipt)}
              chargeReference={t.charge_reference}
            />
          </Panel>
        ) : null}

        <Panel title="Status history">
          <ol className="grid gap-3">
            {t.events.map((e, i) => (
              <li key={i} className="flex flex-wrap items-baseline gap-x-3 text-sm">
                <DateTime value={e.occurred_at} />
                <Status value={e.to_status} />
                <span className="text-muted">
                  {e.from_status ? `from ${e.from_status.replace(/_/g, " ")}` : "recorded"}, by {actorLabel(e.actor)}
                </span>
              </li>
            ))}
          </ol>
        </Panel>

        {t.raw_payload ? (
          <Panel title="Message from M-Pesa">
            <p className="mb-3 text-[0.8125rem] text-muted">As received, with phone numbers masked.</p>
            <pre className="overflow-x-auto rounded-md bg-wash p-4 font-mono text-[0.75rem] leading-relaxed text-ink-2">{prettyJSON(t.raw_payload)}</pre>
          </Panel>
        ) : null}
      </div>
    </>
  );
}
