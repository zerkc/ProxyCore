export const CONTROL_OPERATIONS = [
  "stage",
  "validate",
  "promote",
  "reload",
  "health",
  "rollback",
] as const;

export type ControlOperation = (typeof CONTROL_OPERATIONS)[number];
