import type { Selected } from "@naatre/sdk";
import {
  createGetAccountHandler,
  type GetAccountHandler,
  type GetAccountHandlerOutput,
  type GetAccountResult,
} from "@naatre/sdk/generated";

const handler: GetAccountHandler = async (input, context) => ({
  profile: {
    display: `${context.tenant}:${input.id}`,
    ...(input.nickname === undefined ? {} : { nickname: input.nickname }),
  },
});

const binding = createGetAccountHandler(handler);
void binding;

const serverOutput: GetAccountHandlerOutput = { profile: { display: "Ada" } };
const partialClientOutput: GetAccountResult = {
  profile: { state: "present", value: { display: { state: "present", value: "Ada" }, nickname: { state: "missing" } } },
  later: { state: "pending" },
};
void serverOutput;
void partialClientOutput;

// @ts-expect-error server outputs are complete values, never client Selected states
const selectedServerOutput: GetAccountHandlerOutput = { profile: { state: "present" } as Selected<unknown> };
void selectedServerOutput;

// @ts-expect-error required server output fields cannot be omitted
const missingRequiredOutput: GetAccountHandlerOutput = {};
void missingRequiredOutput;
