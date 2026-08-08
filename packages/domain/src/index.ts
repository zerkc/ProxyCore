export const PRODUCT_NAME = "ProxyCore";

export const MVP_RECORD_TYPES = [
  "A",
  "AAAA",
  "CNAME",
  "TXT",
  "MX",
  "SRV",
] as const;

export type MvpRecordType = (typeof MVP_RECORD_TYPES)[number];

export * from "./dns";
export * from "./errors";
export * from "./auth";
export * from "./jobs";
export * from "./model";
export * from "./paths";
export * from "./proxy";
export * from "./snapshot";
