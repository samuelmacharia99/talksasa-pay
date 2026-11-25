import type { ReactNode } from "react";

import { DocsNav } from "@/components/DocsNav";
import { GUIDES } from "@/content/developer-docs";

const guides = GUIDES.map((g) => ({ href: g.slug ? `/developers/${g.slug}` : "/developers", label: g.nav }));
const reference = [
  { href: "/developers/reference", label: "API reference" },
  { href: "/developers/reference#webhook-payload", label: "Webhook payload" },
];

/** Developers: the app API guides from docs/api, readable by every staff role. */
export default function DevelopersLayout({ children }: { children: ReactNode }) {
  return (
    <div className="lg:grid lg:grid-cols-[200px_minmax(0,1fr)] lg:gap-10">
      <aside className="mb-6 border-b border-line pb-2 lg:mb-0 lg:border-0 lg:pb-0">
        <DocsNav guides={guides} reference={reference} />
      </aside>
      <div className="min-w-0">{children}</div>
    </div>
  );
}
