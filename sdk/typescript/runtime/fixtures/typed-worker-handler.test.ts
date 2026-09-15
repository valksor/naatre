import { NaatreWorkerError, defineHandler } from "../server.mjs";
import type { Handler, ValueSchema } from "../server.mjs";

interface GreetInput { readonly name: string }
interface GreetOutput { readonly greeting: string }

const stringSchema = { kind: "scalar", type: "String" } as const satisfies ValueSchema;
const input = {
  kind: "object",
  fields: [{ name: "name", schema: stringSchema, required: true, nullable: false }],
  additionalProperties: false,
} as const satisfies ValueSchema;
const output = {
  kind: "object",
  fields: [{ name: "greeting", schema: stringSchema, required: true, nullable: false }],
  additionalProperties: false,
} as const satisfies ValueSchema;

const greet: Handler<GreetInput, GreetOutput> = async (value) => {
  if (value.name === "reject") throw NaatreWorkerError.application("NAME_REJECTED", "name was rejected");
  return { greeting: `Hello, ${value.name}` };
};

export function createGreetHandler() {
  return defineHandler<GreetInput, GreetOutput>({
    id: "fixture.greet",
    inputSchema: "GreetInput",
    outputSchema: "GreetOutput",
    codec: "naatre.json-1",
    effect: "query",
    requiredCapabilities: [],
    executionProfile: "inline",
    input,
    output,
  }, greet);
}
