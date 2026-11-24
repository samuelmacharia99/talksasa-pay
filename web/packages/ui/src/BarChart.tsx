import { formatKes, formatKesCompact } from "@talksasa/money";

export interface BarDatum {
  /** ISO date YYYY-MM-DD */
  day: string;
  total: number;
}

const W = 720;
const H = 180;
const PAD = { top: 12, right: 8, bottom: 26, left: 48 };

function niceMax(v: number): number {
  if (v <= 0) return 1000;
  const p = 10 ** Math.floor(Math.log10(v));
  const n = v / p;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return step * p;
}

const dayLabel = new Intl.DateTimeFormat("en-KE", { day: "numeric", month: "short", timeZone: "UTC" });

/**
 * Daily collections as bars. Server-rendered SVG with no chart library; the
 * values are also exposed as an accessible table for screen readers.
 */
export function BarChart({ data, caption }: { data: BarDatum[]; caption: string }) {
  const max = niceMax(Math.max(0, ...data.map((d) => d.total)));
  const innerW = W - PAD.left - PAD.right;
  const innerH = H - PAD.top - PAD.bottom;
  const slot = innerW / Math.max(1, data.length);
  const barW = Math.max(2, Math.min(18, slot * 0.62));
  const ticks = [0, max / 2, max];
  const last = data.length - 1;

  return (
    <figure className="m-0">
      <svg viewBox={`0 0 ${W} ${H}`} className="block h-auto w-full" role="img" aria-label={caption}>
        {ticks.map((t) => {
          const y = PAD.top + innerH - (t / max) * innerH;
          return (
            <g key={t}>
              <line x1={PAD.left} x2={W - PAD.right} y1={y} y2={y} stroke="var(--color-line)" strokeWidth={1} />
              <text x={PAD.left - 8} y={y + 4} textAnchor="end" className="fill-faint" fontSize={11}>
                {formatKesCompact(Math.round(t))}
              </text>
            </g>
          );
        })}
        {data.map((d, i) => {
          const h = (d.total / max) * innerH;
          const x = PAD.left + i * slot + (slot - barW) / 2;
          const isToday = i === last;
          return (
            <g key={d.day}>
              <rect
                x={x}
                y={PAD.top + innerH - h}
                width={barW}
                height={Math.max(h, d.total > 0 ? 1.5 : 0)}
                rx={2}
                className={isToday ? "fill-violet" : "fill-[#b9a3e9]"}
              >
                <title>{`${dayLabel.format(new Date(d.day + "T00:00:00Z"))}: ${formatKes(d.total)}`}</title>
              </rect>
              {/* Weekly ticks counted back from today, so the last label never collides with "Today". */}
              {(isToday || ((last - i) % 7 === 0 && last - i >= 7)) && (
                <text x={x + barW / 2} y={H - 8} textAnchor="middle" fontSize={11} className={isToday ? "fill-ink" : "fill-faint"}>
                  {isToday ? "Today" : dayLabel.format(new Date(d.day + "T00:00:00Z"))}
                </text>
              )}
            </g>
          );
        })}
      </svg>
      <figcaption className="sr-only">
        <table>
          <caption>{caption}</caption>
          <tbody>
            {data.map((d) => (
              <tr key={d.day}>
                <th scope="row">{d.day}</th>
                <td>{formatKes(d.total)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </figcaption>
    </figure>
  );
}
