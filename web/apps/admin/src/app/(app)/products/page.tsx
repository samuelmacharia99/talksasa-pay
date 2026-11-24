import { DateTime, Empty, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";
import Link from "next/link";

import { PageHeader, Panel } from "@/components/PageHeader";
import { getMe, load } from "@/lib/api";

import { CreateProductForm } from "./forms";

export const metadata: Metadata = { title: "Products" };

export default async function Products() {
  const [{ items }, me] = await Promise.all([load((c) => c.GET("/products")), getMe()]);
  const canManage = me.permissions.includes("products.manage");
  return (
    <>
      <PageHeader
        title="Products"
        lead="Talksasa's own products that collect through the shared paybill. Each one has an account prefix customers type before their number."
      />
      <div className="grid gap-6">
        <Panel flush>
          {items.length === 0 ? (
            <Empty title="No products yet">{canManage ? "Create the first one below." : "An admin creates products."}</Empty>
          ) : (
            <Table label="Products">
              <thead>
                <tr>
                  <Th>Product</Th>
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
                      <Link href={`/products/${p.id}`} className="font-medium text-ink hover:text-violet hover:underline">
                        {p.name}
                      </Link>
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
          <Panel title="Add a product">
            <p className="mb-5 max-w-2xl text-sm text-ink-2">
              Only for Talksasa’s own business lines. Money collected for a product is Talksasa revenue; this system never pays
              out to a product or collects for third parties.
            </p>
            <CreateProductForm />
          </Panel>
        ) : null}
      </div>
    </>
  );
}
