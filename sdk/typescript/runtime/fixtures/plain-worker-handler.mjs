import { NaatreWorkerError, defineHandler } from "../server.mjs";

const stringSchema = Object.freeze({ kind: "scalar", type: "String" });
const input = Object.freeze({
  kind: "object",
  fields: Object.freeze([Object.freeze({ name: "name", schema: stringSchema, required: true, nullable: false })]),
  additionalProperties: false,
});
const output = Object.freeze({
  kind: "object",
  fields: Object.freeze([Object.freeze({ name: "greeting", schema: stringSchema, required: true, nullable: false })]),
  additionalProperties: false,
});

export function createGreetHandler() {
  return defineHandler({
    id: "fixture.greet",
    inputSchema: "GreetInput",
    outputSchema: "GreetOutput",
    codec: "naatre.json-1",
    effect: "query",
    requiredCapabilities: [],
    executionProfile: "inline",
    input,
    output,
  }, async (value) => {
    if (value.name === "reject") throw NaatreWorkerError.application("NAME_REJECTED", "name was rejected");
    return { greeting: `Hello, ${value.name}` };
  });
}
