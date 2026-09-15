export const stringSchema = Object.freeze({ kind: "scalar", type: "String" });
export const greetInput = objectSchema([
  { name: "name", schema: stringSchema, required: true, nullable: false },
]);
export const greetOutput = objectSchema([
  { name: "greeting", schema: stringSchema, required: true, nullable: false },
]);

export function handlerDefinition(id, input, output) {
  return {
    id,
    inputSchema: `${id}.input`,
    outputSchema: `${id}.output`,
    codec: "naatre.json-1",
    effect: "query",
    requiredCapabilities: [],
    executionProfile: "inline",
    input,
    output,
  };
}

function objectSchema(fields) {
  return Object.freeze({ kind: "object", fields: Object.freeze(fields.map(Object.freeze)), additionalProperties: false });
}
