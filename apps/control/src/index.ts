export const CONTROL_OPERATIONS = [
  "stage",
  "validate",
  "promote",
  "reload",
  "health",
  "rollback",
] as const;

export type ControlOperation = (typeof CONTROL_OPERATIONS)[number];

export * from "./protocol";
export * from "./service";
export * from "./transport";
