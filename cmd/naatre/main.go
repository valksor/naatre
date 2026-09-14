package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/schema"
	"github.com/valksor/naatre/sdk/go/sdkgen"
	"github.com/valksor/naatre/tooling"
)

const (
	exitOK         = 0
	exitDiagnostic = 1
	exitUsage      = 2
	exitIO         = 3
	exitInternal   = 4
	maximumInput   = 8 << 20
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr)
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "format":
		return runDocumentTransform(args[1:], stdout, stderr, tooling.FormatDocument)
	case "canonicalize":
		return runDocumentTransform(args[1:], stdout, stderr, tooling.CanonicalizeDocument)
	case "hash":
		return runHash(args[1:], stdout, stderr)
	case "schema":
		return runSchema(args[1:], stdout, stderr)
	case "generate":
		return runGenerate(args[1:], stdout, stderr)
	case "manifest":
		return runManifest(args[1:], stdout, stderr)
	case "compatibility":
		return runCompatibility(args[1:], stdout, stderr)
	case "explain":
		return runExplain(args[1:], stdout, stderr)
	case "mock":
		return runMock(args[1:], stdout, stderr)
	case "conformance":
		return runConformance(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		return usage(stdout)
	default:
		return usage(stderr)
	}
}

func usage(output io.Writer) int {
	_, _ = fmt.Fprintln(output, "usage: naatre <validate|format|canonicalize|hash|schema|generate|manifest|compatibility|explain|mock|conformance>")
	return exitUsage
}

func runValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	documentPath := flags.String("document", "", "operation document")
	schemaPath := flags.String("schema", "", "offline schema bundle")
	operation := flags.String("operation", "", "operation name")
	if flags.Parse(args) != nil || *documentPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	document, status := readInput(*documentPath, stderr)
	if status != exitOK {
		return status
	}
	options := tooling.ValidateOptions{Operation: *operation}
	if *schemaPath != "" {
		schemaDocument, schemaStatus := readSchema(*schemaPath, stderr)
		if schemaStatus != exitOK {
			return schemaStatus
		}
		options.Schema = &schemaDocument
	}
	report := tooling.ValidateDocument(document, options)
	if writeJSON(stdout, report) != nil {
		return exitIO
	}
	if len(report.Diagnostics) != 0 {
		return exitDiagnostic
	}
	return exitOK
}

func runDocumentTransform(args []string, stdout, stderr io.Writer, transform func([]byte) ([]byte, error)) int {
	flags := flag.NewFlagSet("document", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	documentPath := flags.String("document", "", "operation document")
	if flags.Parse(args) != nil || *documentPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	input, status := readInput(*documentPath, stderr)
	if status != exitOK {
		return status
	}
	output, err := transform(input)
	if err != nil {
		return writeFailure(stderr, err)
	}
	if _, err := stdout.Write(output); err != nil {
		return exitIO
	}
	return exitOK
}

func runHash(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("hash", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	documentPath := flags.String("document", "", "operation document")
	if flags.Parse(args) != nil || *documentPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	input, status := readInput(*documentPath, stderr)
	if status != exitOK {
		return status
	}
	digest, err := tooling.HashDocument(input)
	if err != nil {
		return writeFailure(stderr, err)
	}
	if writeJSON(stdout, digest) != nil {
		return exitIO
	}
	return exitOK
}

func runSchema(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr)
	}
	switch args[0] {
	case "export":
		return runSchemaExport(args[1:], stdout, stderr)
	case "diff":
		return runSchemaDiff(args[1:], stdout, stderr)
	default:
		return usage(stderr)
	}
}

func runSchemaExport(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("schema export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("schema", "", "offline schema bundle")
	if flags.Parse(args) != nil || *path == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	input, status := readInput(*path, stderr)
	if status != exitOK {
		return status
	}
	output, err := tooling.ExportSchema(input)
	if err != nil {
		return writeFailure(stderr, err)
	}
	_, err = stdout.Write(append(output, '\n'))
	if err != nil {
		return exitIO
	}
	return exitOK
}

func runSchemaDiff(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("schema diff", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	beforePath := flags.String("before", "", "previous schema")
	afterPath := flags.String("after", "", "proposed schema")
	if flags.Parse(args) != nil || *beforePath == "" || *afterPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	before, status := readInput(*beforePath, stderr)
	if status != exitOK {
		return status
	}
	after, status := readInput(*afterPath, stderr)
	if status != exitOK {
		return status
	}
	diff, err := tooling.DiffSchemas(before, after)
	if err != nil {
		return writeFailure(stderr, err)
	}
	if writeJSON(stdout, diff) != nil {
		return exitIO
	}
	return exitOK
}

func runGenerate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	language := flags.String("language", "reference", "reference or go")
	modelPath := flags.String("model", "", "generator model")
	referencePath := flags.String("reference", "", "canonical reference output")
	outputPath := flags.String("out", "", "output file or directory")
	if flags.Parse(args) != nil || *modelPath == "" || *outputPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	model, status := readInput(*modelPath, stderr)
	if status != exitOK {
		return status
	}
	switch *language {
	case "reference":
		output, err := generator.Generate(model)
		if err != nil {
			return writeFailure(stderr, err)
		}
		return writeArtifact(*outputPath, output, stderr)
	case "go":
		if *referencePath == "" {
			return usage(stderr)
		}
		reference, referenceStatus := readInput(*referencePath, stderr)
		if referenceStatus != exitOK {
			return referenceStatus
		}
		artifacts, err := sdkgen.Generate(model, reference)
		if err != nil {
			return writeFailure(stderr, err)
		}
		if err := os.MkdirAll(*outputPath, 0o755); err != nil {
			return writeIOFailure(stderr)
		}
		if status := writeArtifact(filepath.Join(*outputPath, "operations.go"), artifacts.Source, stderr); status != exitOK {
			return status
		}
		return writeArtifact(filepath.Join(*outputPath, "operations.json"), artifacts.Manifest, stderr)
	default:
		return usage(stderr)
	}
}

func runManifest(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	documentPath := flags.String("document", "", "operation document")
	if flags.Parse(args) != nil || *documentPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	input, status := readInput(*documentPath, stderr)
	if status != exitOK {
		return status
	}
	manifest, err := tooling.BuildManifest(input)
	if err != nil {
		return writeFailure(stderr, err)
	}
	output, err := manifest.CanonicalJSON()
	if err != nil {
		return writeFailure(stderr, err)
	}
	_, err = stdout.Write(append(output, '\n'))
	if err != nil {
		return exitIO
	}
	return exitOK
}

func runCompatibility(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("compatibility", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	schemaPath := flags.String("schema", "", "proposed schema")
	manifestPath := flags.String("manifest", "", "operation manifest")
	if flags.Parse(args) != nil || *schemaPath == "" || *manifestPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	schemaDocument, status := readSchema(*schemaPath, stderr)
	if status != exitOK {
		return status
	}
	manifestInput, status := readInput(*manifestPath, stderr)
	if status != exitOK {
		return status
	}
	manifest, err := tooling.LoadManifest(manifestInput)
	if err != nil {
		return writeFailure(stderr, err)
	}
	report := tooling.CheckCompatibility(schemaDocument, manifest)
	if writeJSON(stdout, report) != nil {
		return exitIO
	}
	if !report.Compatible {
		return exitDiagnostic
	}
	return exitOK
}

func runExplain(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("explain", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	schemaPath := flags.String("schema", "", "offline schema")
	documentPath := flags.String("document", "", "operation document")
	operation := flags.String("operation", "", "operation name")
	if flags.Parse(args) != nil || *schemaPath == "" || *documentPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	schemaDocument, status := readSchema(*schemaPath, stderr)
	if status != exitOK {
		return status
	}
	document, status := readInput(*documentPath, stderr)
	if status != exitOK {
		return status
	}
	report := tooling.Explain(schemaDocument, document, *operation)
	if writeJSON(stdout, report) != nil {
		return exitIO
	}
	if len(report.Rejections) != 0 {
		return exitDiagnostic
	}
	return exitOK
}

func runMock(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mock", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	schemaPath := flags.String("schema", "", "offline schema")
	documentPath := flags.String("document", "", "operation document")
	operation := flags.String("operation", "", "operation name")
	seed := flags.Uint64("seed", 1, "deterministic seed")
	scenario := flags.String("scenario", string(tooling.MockSuccess), "mock scenario")
	if flags.Parse(args) != nil || *schemaPath == "" || *documentPath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	schemaDocument, status := readSchema(*schemaPath, stderr)
	if status != exitOK {
		return status
	}
	document, status := readInput(*documentPath, stderr)
	if status != exitOK {
		return status
	}
	result, err := tooling.GenerateMock(schemaDocument, document, *operation, *seed, tooling.MockScenario(*scenario))
	if err != nil {
		return writeFailure(stderr, err)
	}
	if writeJSON(stdout, result) != nil {
		return exitIO
	}
	return exitOK
}

func runConformance(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("conformance", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fixturePath := flags.String("fixture", "", "tooling conformance fixture")
	if flags.Parse(args) != nil || *fixturePath == "" || flags.NArg() != 0 {
		return usage(stderr)
	}
	input, status := readInput(*fixturePath, stderr)
	if status != exitOK {
		return status
	}
	if err := tooling.ValidateConformanceFixture(input); err != nil {
		return writeFailure(stderr, err)
	}
	return writeJSONStatus(stdout, map[string]any{"profile": tooling.ToolingProfile, "status": "pass"})
}

func readSchema(path string, stderr io.Writer) (schema.Document, int) {
	input, status := readInput(path, stderr)
	if status != exitOK {
		return schema.Document{}, status
	}
	document, err := schema.ParseDocument(input, schema.ImportOptions{})
	if err != nil {
		return schema.Document{}, writeFailure(stderr, err)
	}
	return document, exitOK
}

func readInput(path string, stderr io.Writer) ([]byte, int) {
	file, err := os.Open(path)
	if err != nil {
		return nil, writeIOFailure(stderr)
	}
	defer func() { _ = file.Close() }()
	input, err := io.ReadAll(io.LimitReader(file, maximumInput+1))
	if err != nil || len(input) > maximumInput {
		return nil, writeIOFailure(stderr)
	}
	return input, exitOK
}

func writeArtifact(path string, content []byte, stderr io.Writer) int {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".naatre-*")
	if err != nil {
		return writeIOFailure(stderr)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err = temporary.Write(content); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporaryPath, path)
	}
	if err != nil {
		return writeIOFailure(stderr)
	}
	return exitOK
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeJSONStatus(output io.Writer, value any) int {
	if writeJSON(output, value) != nil {
		return exitIO
	}
	return exitOK
}

func writeFailure(stderr io.Writer, err error) int {
	report := tooling.DiagnosticReport{Version: tooling.DiagnosticVersion, Diagnostics: []tooling.Diagnostic{{
		Phase: "tooling", Code: "TOOL_COMMAND_FAILED", Path: "", Message: sanitizeError(err),
	}}}
	_ = writeJSON(stderr, report)
	return exitDiagnostic
}

func writeIOFailure(stderr io.Writer) int {
	_ = writeJSON(stderr, tooling.DiagnosticReport{Version: tooling.DiagnosticVersion, Diagnostics: []tooling.Diagnostic{{
		Phase: "io", Code: "TOOL_IO", Path: "", Message: "tool input or output failed",
	}}})
	return exitIO
}

func sanitizeError(err error) string {
	if err == nil {
		return "tool command failed"
	}
	return tooling.RedactCredentials(strings.ReplaceAll(err.Error(), "\n", " "))
}
