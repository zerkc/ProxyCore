import { describe, expect, it } from "vitest";
import { MVP_RECORD_TYPES, PRODUCT_NAME } from "./index";

describe("domain baseline", () => {
  it("declares the supported MVP record types", () => {
    expect(PRODUCT_NAME).toBe("ProxyCore");
    expect(MVP_RECORD_TYPES).toEqual([
      "A",
      "AAAA",
      "CNAME",
      "TXT",
      "MX",
      "SRV",
    ]);
  });
});
