import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { spawn } from "node:child_process";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { LSPClient } from "../../editors/vscode/client.mjs";

const binary = process.argv[2];
if (!binary) throw new Error("usage: node conformance/independent/verify-lsp.mjs PATH_TO_NAATRE_LSP");
const root = resolve(import.meta.dirname, "../..");
const schema = resolve(root, "conformance/lsp/schema.naatre.json");
const documentPath = resolve(root, "conformance/lsp/document.naatre.json");
const documentText = await readFile(documentPath, "utf8");
const uri = pathToFileURL(documentPath).href;
const child = spawn(resolve(binary), ["--schema", schema], { cwd: root, stdio: ["pipe", "pipe", "pipe"] });
const client = new LSPClient(child.stdout, child.stdin);
let stderr = "";
child.stderr.on("data", (chunk) => { stderr += chunk; });

try {
  const initialized = await client.request("initialize", {
    capabilities: { general: { positionEncodings: ["utf-16"] } },
    initializationOptions: { schemaRevision: "lsp-fixture-r1" },
  });
  assert.equal(initialized.capabilities.positionEncoding, "utf-16");
  assert.equal(initialized.capabilities.textDocumentSync.change, 2);
  assert.equal(initialized.experimental.profile, "tooling.lsp-1");
  client.notify("initialized");

  const published = new Promise((resolveNotification) => client.once("textDocument/publishDiagnostics", resolveNotification));
  client.notify("textDocument/didOpen", { textDocument: {
    uri, languageId: "naatre", version: 1, text: documentText, schemaRevision: "lsp-fixture-r1",
  }});
  const initialDiagnostics = await published;
  assert.equal(initialDiagnostics.uri, uri);
  assert.deepEqual(initialDiagnostics.diagnostics, []);

  const name = locate(documentText, '"name": "name"', 10);
  const completion = await client.request("textDocument/completion", at(uri, name));
  assert.ok(completion.items.some((item) => item.label === "name"));
  const hover = await client.request("textDocument/hover", at(uri, { ...name, character: name.character + 2 }));
  assert.match(hover.contents.value, /Display name/);

  const account = locate(documentText, '"name": "account"', 10);
  const definition = await client.request("textDocument/definition", at(uri, { ...account, character: account.character + 3 }));
  assert.match(definition.uri, /schema\.naatre\.json$/);
  const rename = await client.request("textDocument/rename", { ...at(uri, name), newName: "displayName" });
  assert.ok(rename.changes[uri].some((edit) => edit.newText === "displayName"));

  await assert.rejects(
    client.request("initialize", { initializationOptions: { schemaRevision: "other" } }),
    (error) => error.code === "LSP_SCHEMA_REVISION_MISMATCH",
  );
  await assert.rejects(client.request("workspace/symbol", {}), (error) => error.code === "LSP_METHOD_NOT_FOUND");
  const pull = await client.request("textDocument/diagnostic", { textDocument: { uri } });
  assert.equal(pull.kind, "full");

  await client.request("shutdown");
  client.notify("exit");
  child.stdin.end();
  const exitCode = await new Promise((resolveExit) => child.once("exit", resolveExit));
  assert.equal(exitCode, 0, stderr);
  process.stdout.write(JSON.stringify({ profile: "tooling.lsp-1", client: "vscode-node", status: "passed", schemaRevision: "lsp-fixture-r1" }) + "\n");
} catch (error) {
  child.kill();
  throw error;
}

function at(uri, position) {
  return { textDocument: { uri }, position };
}

function locate(text, fragment, within) {
  const offset = text.indexOf(fragment) + within;
  assert.ok(offset >= within, `missing fixture fragment ${fragment}`);
  const before = text.slice(0, offset);
  const lines = before.split("\n");
  return { line: lines.length - 1, character: [...lines.at(-1)].length };
}
