export class NaatreClientError extends Error {
  constructor(code, options = {}) {
    super(`Naatre client failed: ${code}`, options.cause === undefined ? undefined : { cause: options.cause });
    this.name = "NaatreClientError";
    this.code = code;
    this.status = options.status ?? 0;
  }
}

export function fail(code, cause) {
  throw new NaatreClientError(code, { cause });
}
