import type { Metadata } from "next";

import { Facts, PageHeader, Panel } from "@/components/PageHeader";
import { requireMe } from "@/lib/api";
import { ROLE } from "@/lib/labels";
import { DateTime } from "@talksasa/ui";

import { PasswordForm } from "./PasswordForm";

export const metadata: Metadata = { title: "Your account" };

export default async function Account() {
  const me = await requireMe();
  return (
    <>
      <PageHeader title="Your account" />
      <div className="grid gap-6 lg:grid-cols-2">
        <Panel title="Profile">
          <Facts
            items={[
              ["Name", me.name],
              ["Email", me.email],
              ["Role", `${ROLE[me.role]?.label}. ${ROLE[me.role]?.can}`],
              ["Session ends", <DateTime key="e" value={me.expires_at} />],
            ]}
          />
          <p className="mt-5 text-sm text-muted">
            Lost your authenticator? Sign in with a recovery code, or ask an admin to reset your sign-in.
          </p>
        </Panel>
        <Panel title="Change password">
          <PasswordForm />
        </Panel>
      </div>
    </>
  );
}
