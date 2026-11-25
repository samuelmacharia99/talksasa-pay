import { DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { getMe, load } from "@/lib/api";
import { actorLabel } from "@/lib/labels";

import { AllowedIPs, IssueKeyForm, RevokeKeyButton, RotateSecretForm, StatusForm, WebhookForm } from "../forms";

export const metadata: Metadata = { title: "App" };

function keyState(k: { revoked_at: string | null; expires_at: string | null }): string {
  if (k.revoked_at) return "revoked";
  if (k.expires_at && new Date(k.expires_at) <= new Date()) return "expired_key";
  return "active";
}

export default async function AppPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const [p, keys, me] = await Promise.all([
    load((c) => c.GET("/apps/{id}", { params: { path: { id } } })),
    load((c) => c.GET("/apps/{id}/api-keys", { params: { path: { id } } })),
    getMe(),
  ]);
  const canManage = me.permissions.includes("apps.manage");
  return (
    <>
      <PageHeader
        back={<Link href="/apps" className="text-violet hover:underline">Apps</Link>}
        title={p.name}
        lead={
          <>
            Customers pay to account numbers starting <span className="font-medium text-ink">{p.account_prefix}-</span>
          </>
        }
        actions={<Status value={p.status} className="text-sm" />}
      />
      <div className="grid gap-6">
        <Panel title="Details">
          <Facts
            items={[
              ["Account prefix", p.account_prefix],
              ["Created", <DateTime key="c" value={p.created_at} />],
              ["App id", <span key="i" className="font-mono text-[0.8125rem]">{p.id}</span>],
            ]}
          />
          <div className="mt-5 flex flex-wrap gap-4 text-sm">
            <Link href={`/charges?app_id=${p.id}`} className="text-violet hover:underline">
              Charges
            </Link>
            <Link href={`/transactions?app_id=${p.id}`} className="text-violet hover:underline">
              Transactions
            </Link>
            <Link href={`/webhooks?app_id=${p.id}`} className="text-violet hover:underline">
              Webhook deliveries
            </Link>
          </div>
        </Panel>

        <Panel title="API keys" flush>
          {keys.items.length === 0 ? (
            <Empty title="No keys yet">The app needs a key to create charges.</Empty>
          ) : (
            <Table label="API keys">
              <thead>
                <tr>
                  <Th>Label</Th>
                  <Th>Key id</Th>
                  <Th>Status</Th>
                  <Th>Allowed IPs</Th>
                  <Th>Last used</Th>
                  <Th>Issued</Th>
                  {canManage ? <Th align="right"><span className="sr-only">Actions</span></Th> : null}
                </tr>
              </thead>
              <tbody>
                {keys.items.map((k) => (
                  <tr key={k.key_id}>
                    <Td className="font-medium">{k.label}</Td>
                    <Td className="font-mono text-[0.8125rem] text-ink-2">{k.key_id}</Td>
                    <Td>
                      <Status value={keyState(k)} />
                    </Td>
                    <Td>
                      <AllowedIPs id={p.id} keyID={k.key_id} current={k.allowed_ips ?? null} canEdit={canManage && keyState(k) === "active"} />
                    </Td>
                    <Td>
                      <DateTime value={k.last_used_at} fallback="Never" />
                    </Td>
                    <Td>
                      <DateTime value={k.created_at} dateOnly />
                      <span className="block text-[0.75rem] text-muted">{actorLabel(k.created_by)}</span>
                    </Td>
                    {canManage ? (
                      <Td align="right">{keyState(k) === "active" ? <RevokeKeyButton id={p.id} keyID={k.key_id} /> : null}</Td>
                    ) : null}
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
          {canManage ? (
            <div className="border-t border-line p-5">
              <IssueKeyForm id={p.id} />
            </div>
          ) : null}
        </Panel>

        {canManage ? (
          <div className="grid gap-6 lg:grid-cols-2">
            <Panel title="Webhook">
              <WebhookForm id={p.id} current={p.webhook_url} />
              <div className="mt-6 border-t border-line pt-5">
                <RotateSecretForm id={p.id} />
              </div>
            </Panel>
            <Panel title={p.status === "active" ? "Suspend" : "Re-activate"}>
              <StatusForm id={p.id} status={p.status} />
            </Panel>
          </div>
        ) : (
          <Panel title="Webhook">
            <p className="text-sm text-ink-2">{p.webhook_url || "No webhook URL set."}</p>
          </Panel>
        )}
      </div>
    </>
  );
}
