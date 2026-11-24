import { Slip } from "./Slip";

export default function Home() {
  return (
    <Slip
      top={
        <>
          <h1 className="font-display text-2xl font-semibold tracking-[-0.015em]">Pay a Talksasa bill</h1>
          <p className="mt-3 text-[0.9375rem] text-ink-2">
            Open the payment link on your bill or in the SMS you received. Each link shows one bill and how to pay it with M-Pesa.
          </p>
        </>
      }
    />
  );
}
