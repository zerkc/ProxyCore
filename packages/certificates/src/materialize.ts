import type { CertificateStatus, DnsRecord } from "@proxycore/domain";

type SecretStore = {
  get(id: string): Promise<string | undefined>;
};

export async function materializeCertificateFiles(
  records: DnsRecord[],
  certificates: CertificateStatus[],
  secretStore: SecretStore,
): Promise<Record<string, string>> {
  const neededIds = new Set(
    records
      .filter(
        (record) =>
          record.enabled &&
          record.proxied &&
          record.proxy?.tlsEnabled &&
          record.proxy.certificateId,
      )
      .map((record) => record.proxy!.certificateId!),
  );
  const files: Record<string, string> = {};
  for (const certificateId of neededIds) {
    const certificate = certificates.find((item) => item.id === certificateId);
    if (!certificate) {
      throw new Error(`Certificate not found: ${certificateId}`);
    }
    if (!certificate.certificatePem) {
      throw new Error(`Certificate PEM is missing: ${certificateId}`);
    }
    if (!certificate.secretId) {
      throw new Error(`Certificate private key is missing: ${certificateId}`);
    }
    const privateKeyPem = await secretStore.get(certificate.secretId);
    if (!privateKeyPem) {
      throw new Error(
        `Certificate key secret not found: ${certificate.secretId}`,
      );
    }
    files[`certs/${certificateId}.crt`] = ensureTrailingNewline(
      certificate.certificatePem,
    );
    files[`certs/${certificateId}.key`] = ensureTrailingNewline(privateKeyPem);
  }
  return files;
}

function ensureTrailingNewline(value: string): string {
  return value.endsWith("\n") ? value : `${value}\n`;
}
