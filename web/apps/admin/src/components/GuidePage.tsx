import { buttonClass } from "@talksasa/ui";
import Link from "next/link";
import type { ReactNode } from "react";

import { GUIDES, type Guide } from "@/content/developer-docs";

import { Markdown, tableOfContents } from "./Markdown";
import { PageHeader } from "./PageHeader";

function href(g: Guide) {
  return g.slug ? `/developers/${g.slug}` : "/developers";
}

/** One guide with an "On this page" list and previous/next links. */
export function GuidePage({ guide, lead, actions }: { guide: Guide; lead?: string; actions?: ReactNode }) {
  const toc = tableOfContents(guide.markdown);
  const i = GUIDES.indexOf(guide);
  const prev = i > 0 ? GUIDES[i - 1] : undefined;
  const next = GUIDES[i + 1];
  return (
    <>
      <PageHeader title={guide.title} lead={lead} actions={actions} />
      <div className="xl:grid xl:grid-cols-[minmax(0,1fr)_200px] xl:gap-10">
        <article className="min-w-0">
          <Markdown source={guide.markdown} />
          <nav aria-label="More guides" className="mt-12 grid max-w-[46rem] gap-3 border-t border-line pt-6 sm:grid-cols-2">
            {prev ? (
              <Link href={href(prev)} className="rounded-lg border border-line bg-surface px-4 py-3 hover:border-line-strong">
                <span className="block text-[0.8125rem] text-muted">Previous</span>
                <span className="font-medium text-ink">{prev.nav}</span>
              </Link>
            ) : (
              <span />
            )}
            {next ? (
              <Link href={href(next)} className="rounded-lg border border-line bg-surface px-4 py-3 text-right hover:border-line-strong">
                <span className="block text-[0.8125rem] text-muted">Next</span>
                <span className="font-medium text-ink">{next.nav}</span>
              </Link>
            ) : (
              <Link href="/developers/reference" className="rounded-lg border border-line bg-surface px-4 py-3 text-right hover:border-line-strong">
                <span className="block text-[0.8125rem] text-muted">Next</span>
                <span className="font-medium text-ink">API reference</span>
              </Link>
            )}
          </nav>
          <p className="mt-6 text-[0.8125rem] text-faint">
            Source: <code className="font-mono">docs/api/{guide.file}</code>
          </p>
        </article>
        {toc.length > 1 ? (
          <aside className="hidden xl:block">
            <div className="sticky top-8">
              <p className="mb-2 text-[0.75rem] font-medium tracking-wide text-faint uppercase">On this page</p>
              <ul className="space-y-1.5 border-l border-line text-[0.8125rem]">
                {toc.map((t) => (
                  <li key={t.id}>
                    <a href={`#${t.id}`} className="-ml-px block border-l border-transparent pl-3 text-muted hover:border-violet hover:text-ink">
                      {t.text}
                    </a>
                  </li>
                ))}
              </ul>
            </div>
          </aside>
        ) : null}
      </div>
    </>
  );
}

export function devActions() {
  return (
    <>
      <Link href="/developers/reference" className={buttonClass("secondary", "sm")}>
        API reference
      </Link>
      <a href="/developers/openapi.yaml" download className={buttonClass("secondary", "sm")}>
        Download OpenAPI
      </a>
    </>
  );
}
