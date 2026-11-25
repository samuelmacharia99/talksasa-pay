import { existsSync, readFileSync } from "node:fs";
import path from "node:path";

import { describe, expect, it } from "vitest";

// @ts-expect-error: plain ESM build script without type declarations
import { build, OUTPUT, REPO_ROOT, reviewerNotes } from "../../scripts/developer-docs.mjs";

import { docHref, GUIDES, REFERENCE, slugify } from "./developer-docs";

const haveSources = existsSync(path.join(REPO_ROOT as string, "docs/api/README.md"));

describe.skipIf(!haveSources)("developer docs module", () => {
  it("matches docs/api and api/app.openapi.yaml (run pnpm --filter @talksasa/admin gen:docs)", () => {
    expect(readFileSync(OUTPUT as string, "utf8")).toBe((build as () => string)());
  });
});

describe("reviewer notes", () => {
  const notes = reviewerNotes as (md: string) => string[];
  it("catches notes left for a reviewer", () => {
    expect(notes("Intro\n> For the lead to verify: 401 or 403?")).toEqual(["2: > For the lead to verify: 401 or 403?"]);
    expect(notes("composer require x  # (verify) tag name")).toHaveLength(1);
    expect(notes("TODO: document cancel")).toHaveLength(1);
  });
  it("ignores ordinary text such as phone placeholders", () => {
    expect(notes("as `07XXXXXXXX` or `+2547XXXXXXXX`; Verify the signature; todo list")).toEqual([]);
  });
  it("finds none in the published guides", () => {
    for (const g of GUIDES) expect(notes(g.markdown), g.file).toEqual([]);
  });
});

describe("developer docs", () => {
  it("has every guide and the reference", () => {
    expect(GUIDES.map((g) => g.slug)).toContain("stk-push");
    expect(GUIDES[0]?.slug).toBe("");
    expect(REFERENCE.operations.some((o) => o.id === "sendSTKPush")).toBe(true);
  });

  it("keeps guide links inside the Developers section", () => {
    expect(docHref("stk-push.md")).toBe("/developers/stk-push");
    expect(docHref("laravel.md#pay-invoice-button")).toBe("/developers/laravel#pay-invoice-button");
    expect(docHref("README.md")).toBe("/developers");
    expect(docHref("../../api/app.openapi.yaml")).toBe("/developers/reference");
    expect(docHref("https://www.rfc-editor.org/rfc/rfc9457")).toBe("https://www.rfc-editor.org/rfc/rfc9457");
    expect(docHref("../runbooks/mpesa.md#development-the-fake-daraja")).toBeNull();
  });

  it("resolves every relative link written in the guides", () => {
    for (const g of GUIDES) {
      for (const [, href] of g.markdown.matchAll(/\]\(([^)\s]+)\)/g)) {
        if (!href || href.includes("runbooks/")) continue;
        expect(docHref(href), `${g.file}: ${href}`).not.toBeNull();
      }
    }
  });

  it("makes GitHub-style anchors", () => {
    expect(slugify("Quick start (10 minutes)")).toBe("quick-start-10-minutes");
    expect(slugify("Pay invoice button")).toBe("pay-invoice-button");
  });
});
