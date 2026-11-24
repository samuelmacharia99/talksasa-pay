import "server-only";

import pino from "pino";

import { env } from "./env";

/**
 * Structured JSON logs. Anything that could carry a credential is redacted
 * by path, and callers log ids and codes, never request bodies.
 */
export const log = pino({
  level: env.TSP_LOG_LEVEL,
  base: { service: "talksasa-pay-admin" },
  timestamp: pino.stdTimeFunctions.isoTime,
  redact: {
    paths: [
      "*.cookie",
      "*.authorization",
      "*.password",
      "*.current_password",
      "*.new_password",
      "*.token",
      "*.session_token",
      "*.challenge",
      "*.code",
      "*.api_key",
      "*.webhook_secret",
      "*.totp_secret",
      "*.enrolment",
      "*.recovery_codes",
      "*.setup_url",
    ],
    censor: "[REDACTED]",
  },
});
