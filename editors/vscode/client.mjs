import { EventEmitter } from "node:events";

// LSPClient is the dependency-free framed client used by the VS Code extension
// and by the checked conformance probe. The editor host owns process creation.
export class LSPClient extends EventEmitter {
  #input;
  #output;
  #buffer = Buffer.alloc(0);
  #nextID = 1;
  #pending = new Map();

  constructor(input, output) {
    super();
    this.#input = input;
    this.#output = output;
    input.on("data", (chunk) => this.#accept(Buffer.from(chunk)));
    input.on("error", (error) => this.#fail(error));
    input.on("end", () => this.#fail(new Error("LSP server closed")));
  }

  request(method, params = {}) {
    const id = this.#nextID++;
    this.#send({ jsonrpc: "2.0", id, method, params });
    return new Promise((resolve, reject) => this.#pending.set(id, { resolve, reject }));
  }

  notify(method, params = {}) {
    this.#send({ jsonrpc: "2.0", method, params });
  }

  cancel(id) {
    this.notify("$/cancelRequest", { id });
  }

  #send(message) {
    const payload = Buffer.from(JSON.stringify(message));
    this.#output.write(`Content-Length: ${payload.length}\r\n\r\n`);
    this.#output.write(payload);
  }

  #accept(chunk) {
    this.#buffer = Buffer.concat([this.#buffer, chunk]);
    for (;;) {
      const split = this.#buffer.indexOf("\r\n\r\n");
      if (split < 0) return;
      const header = this.#buffer.subarray(0, split).toString("ascii");
      const match = /(?:^|\r\n)Content-Length:\s*(\d+)/i.exec(header);
      if (!match) return this.#fail(new Error("missing LSP Content-Length"));
      const length = Number(match[1]);
      const end = split + 4 + length;
      if (this.#buffer.length < end) return;
      const payload = this.#buffer.subarray(split + 4, end);
      this.#buffer = this.#buffer.subarray(end);
      let message;
      try {
        message = JSON.parse(payload.toString("utf8"));
      } catch {
        return this.#fail(new Error("invalid LSP response"));
      }
      this.#dispatch(message);
    }
  }

  #dispatch(message) {
    if (message.method) {
      this.emit("notification", message.method, message.params);
      this.emit(message.method, message.params);
      return;
    }
    const pending = this.#pending.get(message.id);
    if (!pending) return;
    this.#pending.delete(message.id);
    if (message.error) {
      const error = new Error(message.error.message);
      error.code = message.error.data?.code;
      pending.reject(error);
    } else {
      pending.resolve(message.result);
    }
  }

  #fail(error) {
    for (const pending of this.#pending.values()) pending.reject(error);
    this.#pending.clear();
    this.emit("closed", error);
  }
}
