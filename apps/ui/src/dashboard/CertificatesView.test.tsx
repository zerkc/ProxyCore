import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { CertificatesView, type DashboardCertificate } from "./CertificatesView";
import { byTag, mount } from "./testDom";

const certificate: DashboardCertificate = {
  id: "certificate-1",
  hostnames: ["primary.example"],
  issuer: "self-signed",
  challenge: "internal",
  environment: "production",
  status: "active",
  expiresAt: "2030-01-01T00:00:00Z",
};

describe("CertificatesView", () => {
  it("keeps ordinary inventory controls without the legacy generic trust-CA button", async () => {
    const container = await mount(
      createElement(CertificatesView, {
        certificates: [certificate],
        onRefresh: vi.fn().mockResolvedValue(undefined),
        onMessage: vi.fn(),
        onError: vi.fn(),
      }),
    );

    expect(container.textContent).toContain("certificate inventory");
    expect(container.textContent).toContain("Refresh inventory");
    expect(container.textContent).not.toContain("Download trust CA");
    expect(byTag(container, "BUTTON").some((button) => button.textContent.includes("trust"))).toBe(false);
  });

  it("shows a neutral fallback for malformed expiry while preserving valid and expired labels", async () => {
    const container = await mount(
      createElement(CertificatesView, {
        certificates: [
          certificate,
          {
            ...certificate,
            id: "certificate-expired",
            expiresAt: "2000-01-01T00:00:00Z",
          },
          {
            ...certificate,
            id: "certificate-invalid",
            expiresAt: "not-a-date",
          },
        ],
        onRefresh: vi.fn().mockResolvedValue(undefined),
        onMessage: vi.fn(),
        onError: vi.fn(),
      }),
    );

    expect(container.textContent).toContain("days left");
    expect(container.textContent).toContain("Expired ·");
    expect(container.textContent).toContain("Expiry unavailable");
    expect(container.textContent).not.toContain("Invalid Date");
    expect(container.textContent).not.toContain("NaN");
  });
});
