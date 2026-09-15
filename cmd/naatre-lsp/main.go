package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/valksor/naatre/schema"
	"github.com/valksor/naatre/tooling/lsp"
)

const maximumSchemaBytes = 8 << 20

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("naatre-lsp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	schemaPath := flags.String("schema", "", "path to one trusted local Naatre schema document")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *schemaPath == "" {
		_, _ = fmt.Fprintln(stderr, "LSP_SCHEMA_REQUIRED")
		return 2
	}
	content, err := readBounded(*schemaPath, maximumSchemaBytes)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "LSP_SCHEMA_UNAVAILABLE")
		return 3
	}
	document, err := schema.ParseDocument(content, schema.ImportOptions{})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "LSP_SCHEMA_INVALID")
		return 1
	}
	server, err := lsp.New(&document, lsp.Limits{})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "LSP_CONFIGURATION_INVALID")
		return 2
	}
	if err := server.Serve(context.Background(), stdin, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "LSP_TRANSPORT_FAILED")
		return 4
	}
	return 0
}

func readBounded(path string, maximumBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if int64(len(content)) > maximumBytes {
		return nil, fmt.Errorf("schema exceeds limit")
	}
	return content, nil
}
