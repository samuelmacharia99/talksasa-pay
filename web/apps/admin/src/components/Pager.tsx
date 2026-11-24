import { buttonClass } from "@talksasa/ui";
import Link from "next/link";

/**
 * Keyset pagination: "Newer" goes back to the first page (cursors only move
 * forward), "Older" follows next_cursor. Filters are preserved.
 */
export function Pager({ basePath, params, nextCursor }: { basePath: string; params: Record<string, string | undefined>; nextCursor?: string | undefined }) {
  const hasCursor = Boolean(params.cursor);
  if (!hasCursor && !nextCursor) return null;
  const href = (cursor?: string) => {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) if (v && k !== "cursor") q.set(k, v);
    if (cursor) q.set("cursor", cursor);
    const s = q.toString();
    return s ? `${basePath}?${s}` : basePath;
  };
  return (
    <div className="flex items-center justify-end gap-2 border-t border-line px-5 py-3">
      {hasCursor ? (
        <Link href={href()} className={buttonClass("ghost", "sm")}>
          Back to newest
        </Link>
      ) : null}
      {nextCursor ? (
        <Link href={href(nextCursor)} className={buttonClass("secondary", "sm")}>
          Older
        </Link>
      ) : null}
    </div>
  );
}
