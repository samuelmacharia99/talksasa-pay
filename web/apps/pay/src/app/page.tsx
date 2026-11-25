import { Slip } from "./Slip";

export default function Home() {
  return (
    <Slip
      top={
        <>
          <h1 className="font-display text-2xl font-semibold tracking-[-0.015em]">Pay with Talksasa</h1>
          <p className="mt-3 text-[0.9375rem] text-ink-2">
            Open the payment link you were sent by SMS, email or on your invoice. Each link shows one payment and how to make it with M-Pesa.
          </p>
        </>
      }
    />
  );
}
