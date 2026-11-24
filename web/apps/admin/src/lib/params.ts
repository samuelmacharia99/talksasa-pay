/** Reads optional string search params, dropping empty and array values. */
export function pick<K extends string>(sp: Record<string, string | string[] | undefined>, keys: readonly K[]): Record<K, string | undefined> {
  const out = {} as Record<K, string | undefined>;
  for (const k of keys) {
    const v = sp[k];
    out[k] = typeof v === "string" && v.trim() !== "" ? v.trim() : undefined;
  }
  return out;
}

export function anySet(p: Record<string, string | undefined>): boolean {
  return Object.entries(p).some(([k, v]) => k !== "cursor" && v !== undefined);
}
