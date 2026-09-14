package main

import (
	"fmt"
	"io"
	"os"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/sdk/go/sdkgen"
)

func main() {
	if status := run(os.Args[1:], os.Stderr); status != 0 {
		os.Exit(status)
	}
}

func run(arguments []string, stderr io.Writer) int {
	if len(arguments) != 3 {
		_, _ = fmt.Fprintln(stderr, "usage: naatre-go-sdk-generator MODEL REFERENCE OUTPUT_ROOT")
		return 2
	}
	model, err := os.ReadFile(arguments[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "Go SDK generator model could not be read")
		return 1
	}
	reference, err := os.ReadFile(arguments[1])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "Go SDK generator reference could not be read")
		return 1
	}
	artifacts, err := sdkgen.Generate(model, reference)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := generator.WriteArtifacts(arguments[2], []generator.Artifact{
		{Path: "operations.go", Content: artifacts.Source},
		{Path: "operations.json", Content: artifacts.Manifest},
	}); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
