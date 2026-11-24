import { Wordmark } from "@talksasa/ui";
import type { ReactNode } from "react";

import { NavLink } from "@/components/NavLink";
import { requireMe } from "@/lib/api";

import { signOut } from "../(auth)/actions";

const ROLE_LABEL: Record<string, string> = {
  viewer: "Viewer",
  finance: "Finance",
  approver: "Approver",
  admin: "Admin",
};

const timeFmt = new Intl.DateTimeFormat("en-KE", { timeZone: "Africa/Nairobi", hour: "2-digit", minute: "2-digit", hour12: false });

export default async function AppLayout({ children }: { children: ReactNode }) {
  const me = await requireMe();
  const canManageUsers = me.permissions.includes("users.manage");
  return (
    <div className="lg:grid lg:min-h-dvh lg:grid-cols-[232px_1fr]">
      <aside className="relative bg-ink text-white lg:sticky lg:top-0 lg:flex lg:h-dvh lg:flex-col">
        <span aria-hidden className="tsp-tail absolute inset-y-0 right-0 hidden w-1 lg:block" />
        <span aria-hidden className="tsp-tail-x absolute inset-x-0 bottom-0 h-0.5 lg:hidden" />
        <div className="flex items-center justify-between px-5 pt-5 pb-3 lg:pb-6">
          <Wordmark onDark />
        </div>
        <nav aria-label="Main" className="flex gap-1 overflow-x-auto px-3 pb-3 lg:flex-1 lg:flex-col lg:overflow-visible lg:pb-0">
          <NavLink href="/">Overview</NavLink>
          <NavLink href="/transactions">Transactions</NavLink>
          <NavLink href="/suspense">Unmatched payments</NavLink>
          <NavLink href="/requests">Payment requests</NavLink>
          <NavLink href="/payers">Payers</NavLink>
          <NavLink href="/products">Products</NavLink>
          <NavLink href="/audit">Audit log</NavLink>
          {canManageUsers ? <NavLink href="/staff">Staff</NavLink> : null}
        </nav>
        <div className="hidden border-t border-white/10 px-5 py-4 text-sm lg:block">
          <a href="/account" className="block font-medium text-white hover:underline">
            {me.name}
          </a>
          <p className="mt-0.5 text-white/55">
            {ROLE_LABEL[me.role] ?? me.role}. Session ends {timeFmt.format(new Date(me.expires_at))}
          </p>
          <form action={signOut} className="mt-3">
            <button type="submit" className="text-white/70 hover:text-white">
              Sign out
            </button>
          </form>
        </div>
      </aside>
      <div className="min-w-0">
        <div className="flex items-center justify-end gap-4 border-b border-line bg-surface px-5 py-2.5 text-sm lg:hidden">
          <a href="/account" className="font-medium">
            {me.name}
          </a>
          <form action={signOut}>
            <button type="submit" className="text-muted">
              Sign out
            </button>
          </form>
        </div>
        <main className="mx-auto max-w-[1180px] px-5 py-8 sm:px-8 lg:py-10">{children}</main>
      </div>
    </div>
  );
}
