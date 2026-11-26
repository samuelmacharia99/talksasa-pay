/** Marks the internal Gateway test app that Settings test payments use. */
export function TestAppBadge() {
  return (
    <span
      title="Internal app for Settings test payments. Its money is test money, not a real app's revenue."
      className="ml-2 inline-flex h-5 items-center rounded-sm bg-warn-soft px-1.5 align-middle text-[0.6875rem] font-semibold tracking-wide text-warn uppercase"
    >
      Test
    </span>
  );
}
