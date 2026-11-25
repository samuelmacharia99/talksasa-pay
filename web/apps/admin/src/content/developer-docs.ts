import { GUIDES, OPENAPI_YAML, REFERENCE } from "./developer-docs.generated";

export type Guide = {
  /** URL segment under /developers; "" is the landing page (README.md). */
  slug: string;
  /** File name in docs/api. */
  file: string;
  /** Short label for the guide list. */
  nav: string;
  /** The guide's own H1. */
  title: string;
  markdown: string;
};

export type Field = { name: string; type: string; required: boolean; description: string; constraints: string[] };

export type Operation = {
  id: string;
  method: string;
  path: string;
  summary: string;
  description: string;
  params: Array<{ name: string; in: string; required: boolean; type: string; description: string }>;
  body: { type: string; fields: Field[] } | null;
  responses: Array<{ status: string; description: string; schema: string | null }>;
};

export type ApiReference = {
  title: string;
  version: string;
  description: string;
  basePath: string;
  operations: Operation[];
  webhooks: Operation[];
  schemas: Array<{ name: string; description: string; type: string; fields: Field[] }>;
};

export { GUIDES, OPENAPI_YAML, REFERENCE };

export function guideBySlug(slug: string): Guide | undefined {
  return GUIDES.find((g) => g.slug === slug);
}

const FILE_TO_HREF = new Map(GUIDES.map((g) => [g.file, g.slug ? `/developers/${g.slug}` : "/developers"]));

/**
 * Where a link written in docs/api should point inside the admin panel.
 * Guide links stay in the Developers section, the contract goes to the API
 * reference, external links pass through, and repo-only paths (runbooks,
 * source folders) return null so they render as plain text.
 */
export function docHref(href: string): string | null {
  if (/^(https?:|mailto:)/.test(href)) return href;
  if (href.startsWith("#")) return href;
  const [target = "", hash] = href.split("#", 2);
  const frag = hash ? `#${hash}` : "";
  // product.openapi.yaml is the contract's old name, still linked from older guides.
  if (target.endsWith("app.openapi.yaml") || target.endsWith("product.openapi.yaml")) return "/developers/reference";
  if (target.endsWith("clients/php")) return "/developers/laravel";
  const page = FILE_TO_HREF.get(target.replace(/^\.\//, ""));
  return page ? page + frag : null;
}

/** GitHub-style heading anchor ("Quick start (10 minutes)" → "quick-start-10-minutes"). */
export function slugify(text: string): string {
  return text
    .toLowerCase()
    .replace(/<[^>]+>/g, "")
    .replace(/[`*_~]/g, "")
    .replace(/[^\p{L}\p{N}\s-]/gu, "")
    .trim()
    .replace(/\s+/g, "-");
}
