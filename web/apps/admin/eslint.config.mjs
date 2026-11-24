import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

const config = [
  ...nextVitals,
  ...nextTs,
  { ignores: [".next/**", "next-env.d.ts"] },
  {
    rules: {
      // Secrets and session tokens must never be logged from the browser.
      "no-console": ["error", { allow: ["error"] }],
      // Server Actions receive (previousState, formData); unused ones are named _.
      "@typescript-eslint/no-unused-vars": ["warn", { argsIgnorePattern: "^_", varsIgnorePattern: "^_" }],
    },
  },
];
export default config;
