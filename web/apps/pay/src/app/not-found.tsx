import { Slip } from "./Slip";

export default function NotFound() {
  return (
    <Slip
      top={
        <>
          <h1 className="font-display text-2xl font-semibold tracking-[-0.015em]">This payment link isn’t valid</h1>
          <p className="mt-3 text-[0.9375rem] text-ink-2">
            Check that you opened the full link you were sent. Links stop working if the business that issued them asks
            us to. If you need to pay, ask them for a new link.
          </p>
        </>
      }
    />
  );
}
