/** All times are shown in Kenyan time, whatever the viewer's machine says. */
export const TZ = "Africa/Nairobi";

const dateTime = new Intl.DateTimeFormat("en-KE", {
  timeZone: TZ,
  day: "numeric",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});
const dateOnly = new Intl.DateTimeFormat("en-KE", { timeZone: TZ, day: "numeric", month: "short", year: "numeric" });

export function formatDateTime(iso: string): string {
  return dateTime.format(new Date(iso));
}

export function formatDate(iso: string): string {
  return dateOnly.format(new Date(iso));
}

export function DateTime({ value, dateOnly: onlyDate = false, fallback = "—" }: { value: string | null | undefined; dateOnly?: boolean; fallback?: string }) {
  if (!value) return <span className="text-faint">{fallback}</span>;
  return (
    <time dateTime={value} className="num whitespace-nowrap" title={`${new Date(value).toISOString()} (UTC)`}>
      {onlyDate ? formatDate(value) : formatDateTime(value)}
    </time>
  );
}
