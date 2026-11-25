// Builds src/content/developer-docs.generated.ts from the repository's
// app API guides (docs/api/*.md) and contract (api/app.openapi.yaml).
//
// The admin image is built from web/ only, so the docs are copied in as a
// committed module. docs/api stays the source of truth: edit the markdown,
// then run `pnpm --filter @talksasa/admin gen:docs`. A unit test fails when
// the generated module is out of date.
//
//   node scripts/developer-docs.mjs          write the module
//   node scripts/developer-docs.mjs --check  exit 1 if it is out of date

import { existsSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import yaml from "js-yaml";

const here = path.dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = path.resolve(here, "../../../..");
export const OUTPUT = path.resolve(here, "../src/content/developer-docs.generated.ts");

/** Guides in reading order. README.md is the Developers landing page. */
const GUIDES = [
  { slug: "", file: "README.md", nav: "Overview" },
  { slug: "authentication", file: "authentication.md", nav: "Authentication" },
  { slug: "idempotency-and-retries", file: "idempotency-and-retries.md", nav: "Idempotency and retries" },
  { slug: "stk-push", file: "stk-push.md", nav: "M-Pesa prompts" },
  { slug: "webhooks", file: "webhooks.md", nav: "Webhooks" },
  { slug: "events-feed", file: "events-feed.md", nav: "Events feed" },
  // charges.md replaces money-rules.md in the gateway redesign; use whichever
  // docs/api has so the generator works before and after that lands.
  { slug: "charges", file: "charges.md", nav: "Charges", fallback: { slug: "money-rules", file: "money-rules.md", nav: "Money rules" } },
  { slug: "errors", file: "errors.md", nav: "Errors" },
  { slug: "laravel", file: "laravel.md", nav: "Laravel (PHP SDK)" },
];

const METHODS = ["get", "put", "post", "patch", "delete"];

function refName(ref) {
  return ref.split("/").pop();
}

function resolve(spec, node) {
  if (node && typeof node === "object" && typeof node.$ref === "string") {
    const parts = node.$ref.replace(/^#\//, "").split("/");
    return parts.reduce((o, k) => o?.[k], spec);
  }
  return node;
}

/** A short, readable type: `PaymentRequest`, `string[]`, `"open" | "paid"`. */
function typeOf(schema) {
  if (!schema || typeof schema !== "object") return "any";
  if (schema.$ref) return refName(schema.$ref);
  if (schema.enum) return schema.enum.map((v) => JSON.stringify(v)).join(" | ");
  if (schema.const !== undefined) return JSON.stringify(schema.const);
  for (const k of ["oneOf", "anyOf", "allOf"]) {
    if (schema[k]) return schema[k].map(typeOf).join(k === "allOf" ? " & " : " | ");
  }
  let t = Array.isArray(schema.type) ? schema.type.join(" | ") : schema.type;
  if (t === "array") return `${typeOf(schema.items)}[]`;
  if (!t) t = schema.properties ? "object" : "any";
  if (schema.format) t += ` (${schema.format})`;
  return t;
}

function constraints(schema) {
  if (!schema || typeof schema !== "object") return [];
  const out = [];
  if (schema.minimum !== undefined) out.push(`min ${schema.minimum}`);
  if (schema.maximum !== undefined) out.push(`max ${schema.maximum}`);
  if (schema.minLength !== undefined) out.push(`min length ${schema.minLength}`);
  if (schema.maxLength !== undefined) out.push(`max length ${schema.maxLength}`);
  if (schema.maxItems !== undefined) out.push(`max ${schema.maxItems} items`);
  if (schema.pattern) out.push(`pattern \`${schema.pattern}\``);
  if (schema.default !== undefined) out.push(`default ${JSON.stringify(schema.default)}`);
  return out;
}

function fields(spec, schema) {
  const s = resolve(spec, schema);
  if (!s || !s.properties) return [];
  const required = new Set(s.required ?? []);
  return Object.entries(s.properties).map(([name, p]) => ({
    name,
    type: typeOf(p),
    required: required.has(name),
    description: (resolve(spec, p)?.description ?? p.description ?? "").trim(),
    constraints: constraints(p),
  }));
}

function jsonSchemaOf(content) {
  return content?.["application/json"]?.schema ?? content?.["application/problem+json"]?.schema;
}

function operation(spec, method, pathName, pathItem, op) {
  const params = [...(pathItem.parameters ?? []), ...(op.parameters ?? [])].map((p) => {
    const r = resolve(spec, p);
    return {
      name: r.name,
      in: r.in,
      required: Boolean(r.required),
      type: typeOf(r.schema),
      description: (r.description ?? "").trim(),
    };
  });
  const body = op.requestBody ? jsonSchemaOf(resolve(spec, op.requestBody).content) : undefined;
  const responses = Object.entries(op.responses ?? {}).map(([status, res]) => {
    const r = resolve(spec, res);
    const schema = jsonSchemaOf(r.content);
    return { status, description: (r.description ?? "").trim(), schema: schema ? typeOf(schema) : null };
  });
  return {
    id: op.operationId ?? `${method}-${pathName}`,
    method: method.toUpperCase(),
    path: pathName,
    summary: op.summary ?? "",
    description: (op.description ?? "").trim(),
    params,
    body: body ? { type: typeOf(body), fields: fields(spec, body) } : null,
    responses,
  };
}

function reference(spec) {
  const operations = [];
  for (const [pathName, item] of Object.entries(spec.paths ?? {})) {
    for (const m of METHODS) if (item[m]) operations.push(operation(spec, m, pathName, item, item[m]));
  }
  const webhooks = [];
  for (const [name, item] of Object.entries(spec.webhooks ?? {})) {
    for (const m of METHODS) if (item[m]) webhooks.push({ ...operation(spec, m, name, item, item[m]), id: `webhook-${name}` });
  }
  const schemas = Object.entries(spec.components?.schemas ?? {}).map(([name, s]) => ({
    name,
    description: (s.description ?? "").trim(),
    type: s.properties ? "object" : typeOf(s),
    fields: fields(spec, s),
  }));
  return {
    title: spec.info?.title ?? "",
    version: String(spec.info?.version ?? ""),
    description: (spec.info?.description ?? "").trim(),
    basePath: spec.servers?.[0]?.url ?? "/v1",
    operations,
    webhooks,
    schemas,
  };
}

/**
 * Notes meant for a reviewer, not for app developers. The guides are
 * published to staff as-is, so any of these stops generation.
 */
const REVIEWER_NOTES = [
  /for the lead\b/i,
  /\bto verify before publishing\b/i,
  /\(verify\b/i,
  /\bTODO\b/,
  /\bFIXME\b/,
  /\bTBD\b/,
  /\breviewer note\b/i,
];

/** Lines of `markdown` that look like reviewer notes, as "line: text". */
export function reviewerNotes(markdown) {
  const found = [];
  markdown.split("\n").forEach((line, i) => {
    if (REVIEWER_NOTES.some((re) => re.test(line))) found.push(`${i + 1}: ${line.trim()}`);
  });
  return found;
}

function title(markdown, fallback) {
  const m = /^#\s+(.+)$/m.exec(markdown);
  return m ? m[1].trim() : fallback;
}

/** The generated module's source text. */
export function build(root = REPO_ROOT) {
  const guides = GUIDES.map((entry) => {
    const g = entry.fallback && !existsSync(path.join(root, "docs/api", entry.file)) ? entry.fallback : entry;
    const markdown = readFileSync(path.join(root, "docs/api", g.file), "utf8");
    const notes = reviewerNotes(markdown);
    if (notes.length) {
      throw new Error(`docs/api/${g.file} has reviewer notes; resolve them before publishing:\n  ${notes.join("\n  ")}`);
    }
    return { slug: g.slug, file: g.file, nav: g.nav, title: title(markdown, g.nav), markdown };
  });
  const openapi = readFileSync(path.join(root, "api/app.openapi.yaml"), "utf8");
  const ref = reference(yaml.load(openapi));
  return [
    "// Code generated by scripts/developer-docs.mjs from docs/api/*.md and",
    "// api/app.openapi.yaml. DO NOT EDIT: run `pnpm --filter @talksasa/admin gen:docs`.",
    "",
    'import type { ApiReference, Guide } from "./developer-docs";',
    "",
    `export const GUIDES: Guide[] = ${JSON.stringify(guides, null, 2)};`,
    "",
    `export const REFERENCE: ApiReference = ${JSON.stringify(ref, null, 2)};`,
    "",
    `export const OPENAPI_YAML: string = ${JSON.stringify(openapi)};`,
    "",
  ].join("\n");
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const next = build();
  if (process.argv.includes("--check")) {
    let current = "";
    try {
      current = readFileSync(OUTPUT, "utf8");
    } catch {
      // missing counts as out of date
    }
    if (current !== next) {
      console.error(`${path.relative(process.cwd(), OUTPUT)} is out of date: run pnpm --filter @talksasa/admin gen:docs`);
      process.exit(1);
    }
  } else {
    writeFileSync(OUTPUT, next);
    process.stdout.write(`wrote ${path.relative(process.cwd(), OUTPUT)}\n`);
  }
}
