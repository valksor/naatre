import { createFetchWorkerAdapter, createWorker } from "@naatre/sdk/worker";
import { createGetAccountHandler } from "@naatre/sdk/generated";

const account = createGetAccountHandler(async (input, context) => ({
  profile: {
    display: `${context.tenant}:${input.id}`,
    ...(input.nickname === undefined ? {} : { nickname: input.nickname }),
  },
}));

export const worker = createWorker({
  workerId: "accounts-worker",
  serviceIdentity: "spiffe://example/accounts-worker",
  audience: "naatre-gateway",
  endpoint: "accounts-worker",
  schemaRevision: "schema-generator-r1",
  schemaDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  capabilities: ["unary-1", "cancellation-ack-1"],
  limits: {
    maxInFlight: 32,
    maxRequestBytes: 1 << 20,
    maxResponseBytes: 1 << 20,
    maxStreamFrames: 1024,
    maxStreamBytes: 8 << 20,
  },
  handlers: [account],
  authenticate: async ({ delegatedContext }) => {
    // Verify the audience, expiry, request, handler, and schema bindings here.
    if (delegatedContext === "") throw new Error("delegation rejected");
    return { principal: "authenticated-principal", tenant: "request-tenant" };
  },
});

// Mount this function in any Fetch-compatible host. Runtime-specific process,
// HTTP server, and edge lifecycle adapters are intentionally separate.
export const fetch = createFetchWorkerAdapter(worker);
