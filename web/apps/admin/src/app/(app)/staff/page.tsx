import { DateTime, Status, Table, Td, Th } from "@talksasa/ui";
import type { Metadata } from "next";

import { PageHeader, Panel } from "@/components/PageHeader";
import { load, requireMe } from "@/lib/api";
import { ROLE } from "@/lib/labels";

import { AccountActions, InviteForm, RoleForm } from "./forms";

export const metadata: Metadata = { title: "Staff" };

export default async function Staff() {
  const me = await requireMe("users.manage");
  const { items } = await load((c) => c.GET("/users"));
  return (
    <>
      <PageHeader title="Staff" lead="Who can sign in to this console. Everyone signs in with a password and an authenticator app." />
      <div className="grid gap-6">
        <Panel flush>
          <Table label="Staff accounts">
            <thead>
              <tr>
                <Th>Person</Th>
                <Th>Status</Th>
                <Th>Role</Th>
                <Th>Last sign-in</Th>
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {items.map((u) => {
                const self = u.id === me.id;
                return (
                  <tr key={u.id} className="align-top">
                    <Td>
                      <span className="block font-medium">
                        {u.name}
                        {self ? <span className="ml-2 text-[0.75rem] font-normal text-muted">you</span> : null}
                      </span>
                      <span className="block text-[0.8125rem] text-muted">{u.email}</span>
                    </Td>
                    <Td>
                      <Status value={u.status} />
                    </Td>
                    <Td>{self || u.status === "disabled" ? ROLE[u.role]?.label : <RoleForm id={u.id} current={u.role} />}</Td>
                    <Td>
                      <DateTime value={u.last_login_at} fallback="Never" />
                    </Td>
                    <Td>{self ? null : <AccountActions id={u.id} name={u.name} status={u.status} />}</Td>
                  </tr>
                );
              })}
            </tbody>
          </Table>
        </Panel>
        <Panel title="Invite a staff member">
          <InviteForm />
        </Panel>
      </div>
    </>
  );
}
