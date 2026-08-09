import { InMemoryHttp01ChallengeStore } from "@proxycore/certificates";

const globalState = globalThis as typeof globalThis & {
  __proxycoreHttp01ChallengeStore?: InMemoryHttp01ChallengeStore;
};

export const http01ChallengeStore =
  globalState.__proxycoreHttp01ChallengeStore ??
  (globalState.__proxycoreHttp01ChallengeStore =
    new InMemoryHttp01ChallengeStore());
