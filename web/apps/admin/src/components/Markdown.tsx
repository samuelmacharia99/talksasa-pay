import "server-only";

import { Marked, type Token, type Tokens } from "marked";
import type { ReactNode } from "react";

import { docHref, slugify } from "@/content/developer-docs";

import { CodeBlock } from "./CodeBlock";

export type TocEntry = { id: string; text: string };

function escapeAttr(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;");
}

/** Assigns unique heading ids in document order (a repeat gets "-1", "-2"...). */
function idMaker() {
  const seen = new Map<string, number>();
  return (text: string) => {
    const base = slugify(text) || "section";
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    return n ? `${base}-${n}` : base;
  };
}

function parse(markdown: string) {
  const nextId = idMaker();
  const marked = new Marked({
    gfm: true,
    renderer: {
      heading({ tokens, depth, text }) {
        const inner = this.parser.parseInline(tokens);
        if (depth === 1) return ""; // the page header shows the guide's title
        const id = nextId(text);
        return `<h${depth} id="${id}"><a href="#${id}" class="tsp-anchor">${inner}</a></h${depth}>\n`;
      },
      link({ href, title, tokens }) {
        const inner = this.parser.parseInline(tokens);
        const to = docHref(href);
        if (!to) return inner;
        const external = /^https?:/.test(to);
        const attrs = [`href="${escapeAttr(to)}"`];
        if (title) attrs.push(`title="${escapeAttr(title)}"`);
        if (external) attrs.push('target="_blank" rel="noopener noreferrer"');
        return `<a ${attrs.join(" ")}>${inner}</a>`;
      },
    },
  });
  const tokens = marked.lexer(markdown);

  // Table of contents: the H2s, with the same ids the renderer will assign.
  const tocId = idMaker();
  const toc: TocEntry[] = [];
  for (const t of tokens) {
    if (t.type !== "heading" || t.depth === 1) continue;
    const id = tocId(t.text);
    if (t.depth === 2) toc.push({ id, text: t.text.replace(/[`*_]/g, "") });
  }
  return { marked, tokens, toc };
}

/** The H2 sections of a guide, for an "On this page" list. */
export function tableOfContents(markdown: string): TocEntry[] {
  return parse(markdown).toc;
}

/**
 * Renders one of our own docs/api guides. The markdown is repository content
 * (not user input), so its HTML is trusted. Top-level code blocks become
 * React components with a copy button; everything else is marked's HTML.
 */
export function Markdown({ source, className }: { source: string; className?: string }) {
  const { marked, tokens } = parse(source);
  const out: ReactNode[] = [];
  let run: Token[] = [];
  const flush = () => {
    if (!run.length) return;
    const html = marked.parser(run);
    out.push(<div key={out.length} dangerouslySetInnerHTML={{ __html: html }} />);
    run = [];
  };
  for (const t of tokens) {
    if (t.type === "code") {
      flush();
      out.push(<CodeBlock key={out.length} code={(t as Tokens.Code).text} lang={(t as Tokens.Code).lang ?? ""} />);
    } else {
      run.push(t);
    }
  }
  flush();
  return <div className={className ?? "tsp-prose"}>{out}</div>;
}
