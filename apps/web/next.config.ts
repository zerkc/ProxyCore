import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "standalone",
  transpilePackages: [
    "@proxycore/domain",
    "@proxycore/db",
    "@proxycore/config",
    "@proxycore/crypto",
    "@proxycore/renderers",
    "@proxycore/certificates",
  ],
};

export default nextConfig;
