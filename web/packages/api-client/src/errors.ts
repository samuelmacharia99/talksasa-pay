/** RFC 9457 problem details, as returned by every Go endpoint on error. */
export interface Problem {
  type?: string;
  title?: string;
  status: number;
  detail?: string;
  code?: string;
  request_id?: string;
  /** Per-field validation messages, when the service reports them. */
  errors?: FieldError[];
}

export interface FieldError {
  field: string;
  message: string;
}

/**
 * An error response from the Go service. `detail` is written for end users
 * and never contains internal information, so it is safe to display.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly detail: string;
  readonly requestId: string | undefined;
  readonly fieldErrors: FieldError[];

  constructor(p: Problem) {
    super(p.detail || p.title || `HTTP ${p.status}`);
    this.name = "ApiError";
    this.status = p.status;
    this.code = p.code ?? "error";
    this.detail = p.detail ?? p.title ?? "Something went wrong.";
    this.requestId = p.request_id;
    this.fieldErrors = Array.isArray(p.errors)
      ? p.errors.filter((f): f is FieldError => typeof f?.field === "string" && typeof f?.message === "string")
      : [];
  }
}

/** Builds an ApiError from whatever an error response body turned out to be. */
export function toApiError(status: number, body: unknown): ApiError {
  if (body && typeof body === "object" && "status" in body) {
    return new ApiError(body as Problem);
  }
  return new ApiError({
    status,
    code: status >= 500 ? "upstream_error" : "error",
    detail: status >= 500 ? "The payment service is unavailable. Try again shortly." : "The request failed.",
  });
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError;
}
