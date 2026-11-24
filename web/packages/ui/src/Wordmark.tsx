import { cx } from "./cx";

/**
 * The Talksasa mark with the "talksasa" wordmark in its brand colours
 * (talk: sky blue, sasa: purple) and the product name.
 */
export function Wordmark({
  product = "Pay",
  onDark = false,
  className,
  markSrc = "/brand/talksasa-mark.png",
}: {
  product?: string;
  onDark?: boolean;
  className?: string;
  markSrc?: string;
}) {
  return (
    <span className={cx("inline-flex items-center gap-2.5", className)}>
      {/* eslint-disable-next-line @next/next/no-img-element -- static brand asset, no optimisation needed */}
      <img src={markSrc} alt="" width={28} height={28} className="size-7 shrink-0" />
      <span className="font-display text-[1.05rem] leading-none font-semibold tracking-[-0.01em]">
        <span className={onDark ? "text-[#7cc8ea]" : "text-sky"}>talk</span>
        <span className={onDark ? "text-[#b994f3]" : "text-[#7e44d0]"}>sasa</span>
        <span className={cx("ml-1.5 font-medium", onDark ? "text-white/70" : "text-muted")}>{product}</span>
      </span>
    </span>
  );
}
