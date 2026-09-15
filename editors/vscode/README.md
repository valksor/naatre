# Naatre Language Support for VS Code

This dependency-free desktop extension launches `naatre-lsp` over stdio and
maps its diagnostics, completion, hover, definition, and rename results to VS
Code providers. Install or build `naatre-lsp`, then configure:

```json
{
  "naatre.server": "/absolute/path/to/naatre-lsp",
  "naatre.schema": "/absolute/path/to/schema.naatre.json"
}
```

The schema path is mandatory. The extension never searches for a schema and the
server never fetches a schema URI. See [`docs/lsp.md`](../../docs/lsp.md) for
ownership, lifecycle, runtime evidence, limits, and the complete unsupported
capability inventory.
