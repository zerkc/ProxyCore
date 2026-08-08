export class DomainValidationError extends Error {
  readonly code: string;

  constructor(message: string, code = "DOMAIN_VALIDATION") {
    super(message);
    this.name = "DomainValidationError";
    this.code = code;
  }
}

export function assertDomain(
  condition: unknown,
  message: string,
  code?: string,
): asserts condition {
  if (!condition) {
    throw new DomainValidationError(message, code);
  }
}
