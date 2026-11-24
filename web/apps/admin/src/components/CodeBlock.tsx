import { CopyButton } from "@talksasa/ui";

const LANG_LABEL: Record<string, string> = {
  sh: "Shell",
  bash: "Shell",
  json: "JSON",
  php: "PHP",
  go: "Go",
  http: "HTTP",
  yaml: "YAML",
  env: ".env",
  dotenv: ".env",
  text: "Text",
  js: "JavaScript",
  ts: "TypeScript",
};

/** A dark code panel with its language and a copy button. */
export function CodeBlock({ code, lang }: { code: string; lang: string }) {
  const label = LANG_LABEL[lang.toLowerCase()] ?? (lang || "Code");
  return (
    <figure className="tsp-code my-5 overflow-hidden rounded-lg border border-ink/80 bg-ink">
      <figcaption className="flex items-center justify-between gap-3 border-b border-white/10 py-1.5 pr-1.5 pl-4">
        <span className="font-mono text-[0.75rem] tracking-wide text-white/55 uppercase">{label}</span>
        <CopyButton
          value={code}
          className="h-7 border-white/15 bg-white/5 px-2.5 text-[0.75rem] text-white/80 hover:bg-white/10 hover:text-white"
        />
      </figcaption>
      <pre className="overflow-x-auto px-4 py-3.5 font-mono text-[0.8125rem] leading-relaxed text-[#ece9f7]">
        <code>{code}</code>
      </pre>
    </figure>
  );
}
