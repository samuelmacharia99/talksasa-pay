import { DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { PageHeader, Panel } from "@/components/PageHeader";
import { TestAppBadge } from "@/components/TestAppBadge";
import { getMe, load } from "@/lib/api";

import { CreateAppForm } from "./forms";

export const metadata: Metadata = { title: "Apps" };

export default async function Apps() {
  const [{ items }, me] = await Promise.all([load((c) => c.GET("/apps")), getMe()]);
  const canManage = me.permissions.includes("apps.manage");
  return (
    <>
      <PageHeader
        title="Apps"
        lead="Systems that collect money through the gateway. Each app has its own API keys and webhook, and an account prefix that routes paybill payments to it."
      />
      <div className="grid gap-6">
        <Panel flush>
          {items.length === 0 ? (
            <Empty title="No apps yet">{canManage ? "Create the first one below." : "An admin creates apps."}</Empty>
          ) : (
            <Table label="Apps">
              <thead>
                <tr>
                  <Th>App</Th>
                  <Th>Prefix</Th>
                  <Th>Status</Th>
                  <Th>Events sent to</Th>
                  <Th>Created</Th>
                </tr>
              </thead>
              <tbody>
                {items.map((p) => (
                  <tr key={p.id} className="hover:bg-wash/60">
                    <Td>
                      <Link href={`/apps/${p.id}`} className="font-medium text-ink hover:text-violet hover:underline">
                        {p.name}
                      </Link>
                      {p.gateway_test ? <TestAppBadge /> : null}
                    </Td>
                    <Td className="font-medium">{p.account_prefix}</Td>
                    <Td>
                      <Status value={p.status} />
                    </Td>
                    <Td className="max-w-[18rem] truncate text-ink-2">{p.webhook_url || <span className="text-faint">Not set</span>}</Td>
                    <Td>
                      <DateTime value={p.created_at} dateOnly />
                    </Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Panel>
        {canManage ? (
          <Panel title="Add an app">
            <p className="mb-5 max-w-2xl text-sm text-ink-2">
              The app creates charges with its API key and hears about payments by webhook. Its account prefix is permanent:
              paybill payments whose account number starts with it are routed to the app.
            </p>
            <CreateAppForm />
          </Panel>
        ) : null}
      </div>
    </>
  );
}
