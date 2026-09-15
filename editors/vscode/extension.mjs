import { spawn } from "node:child_process";
import * as vscode from "vscode";
import { LSPClient } from "./client.mjs";

let child;

export async function activate(context) {
  const configuration = vscode.workspace.getConfiguration("naatre");
  const schema = configuration.get("schema");
  if (!schema) return;
  child = spawn(configuration.get("server", "naatre-lsp"), ["--schema", schema], {
    stdio: ["pipe", "pipe", "pipe"],
    windowsHide: true,
  });
  const client = new LSPClient(child.stdout, child.stdin);
  const initialized = await client.request("initialize", {
    processId: process.pid,
    capabilities: { general: { positionEncodings: ["utf-16"] } },
  });
  client.notify("initialized", {});
  const schemaRevision = initialized.experimental.schemaRevision;
  const selector = { language: "naatre", scheme: "file" };
  const diagnostics = vscode.languages.createDiagnosticCollection("naatre");

  client.on("textDocument/publishDiagnostics", ({ uri, diagnostics: values }) => {
    diagnostics.set(vscode.Uri.parse(uri), values.map((item) => {
      const diagnostic = new vscode.Diagnostic(asRange(item.range), item.message, item.severity - 1);
      diagnostic.code = item.code;
      diagnostic.source = item.source;
      diagnostic.tags = item.tags?.map((tag) => tag === 2 ? vscode.DiagnosticTag.Deprecated : tag);
      return diagnostic;
    }));
  });

  const open = (document) => client.notify("textDocument/didOpen", { textDocument: {
    uri: document.uri.toString(), languageId: "naatre", version: document.version,
    text: document.getText(), schemaRevision,
  }});
  for (const document of vscode.workspace.textDocuments.filter((value) => value.languageId === "naatre")) open(document);
  context.subscriptions.push(
    diagnostics,
    vscode.workspace.onDidOpenTextDocument((document) => document.languageId === "naatre" && open(document)),
    vscode.workspace.onDidChangeTextDocument(({ document }) => document.languageId === "naatre" && client.notify("textDocument/didChange", {
      textDocument: { uri: document.uri.toString(), version: document.version, schemaRevision },
      contentChanges: [{ text: document.getText() }],
    })),
    vscode.workspace.onDidCloseTextDocument((document) => document.languageId === "naatre" && client.notify("textDocument/didClose", { textDocument: { uri: document.uri.toString() } })),
    vscode.languages.registerCompletionItemProvider(selector, provider(client, "textDocument/completion", (result) => result.items.map((item) => Object.assign(new vscode.CompletionItem(item.label, item.kind - 1), { detail: item.detail })))),
    vscode.languages.registerHoverProvider(selector, provider(client, "textDocument/hover", (result) => result && new vscode.Hover(result.contents.value, asRange(result.range)))),
    vscode.languages.registerDefinitionProvider(selector, provider(client, "textDocument/definition", (result) => result && new vscode.Location(vscode.Uri.parse(result.uri), asRange(result.range)))),
    vscode.languages.registerRenameProvider(selector, {
      prepareRename: (document, position) => client.request("textDocument/prepareRename", at(document, position)).then((result) => ({ range: asRange(result.range), placeholder: result.placeholder })),
      provideRenameEdits: (document, position, newName) => client.request("textDocument/rename", { ...at(document, position), newName }).then((result) => {
        const edit = new vscode.WorkspaceEdit();
        for (const [uri, edits] of Object.entries(result.changes ?? {})) for (const change of edits) edit.replace(vscode.Uri.parse(uri), asRange(change.range), change.newText);
        return edit;
      }),
    }),
    { dispose: () => {
      void client.request("shutdown").then(() => {
        client.notify("exit");
        child?.stdin.end();
      }).catch(() => child?.kill());
    } },
  );
}

export function deactivate() {
  child?.kill();
}

function provider(client, method, convert) {
  const name = method === "textDocument/completion" ? "provideCompletionItems" : method === "textDocument/hover" ? "provideHover" : "provideDefinition";
  return { [name]: (document, position) => client.request(method, at(document, position)).then(convert) };
}

function at(document, position) {
  return { textDocument: { uri: document.uri.toString() }, position: { line: position.line, character: position.character } };
}

function asRange(range) {
  return new vscode.Range(range.start.line, range.start.character, range.end.line, range.end.character);
}
