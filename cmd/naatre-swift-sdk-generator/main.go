package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valksor/naatre/sdk/swift/sdkgen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("naatre-swift-sdk-generator", flag.ContinueOnError)
	modelPath := flags.String("model", "", "language-neutral generator model")
	referencePath := flags.String("reference", "", "canonical reference output")
	sourcePath := flags.String("source", "", "generated Swift source")
	manifestPath := flags.String("manifest", "", "generated manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *modelPath == "" || *referencePath == "" || *sourcePath == "" || *manifestPath == "" || flags.NArg() != 0 {
		return fmt.Errorf("model, reference, source, and manifest paths are required")
	}
	model, err := os.ReadFile(*modelPath)
	if err != nil {
		return fmt.Errorf("read model: %w", err)
	}
	reference, err := os.ReadFile(*referencePath)
	if err != nil {
		return fmt.Errorf("read reference: %w", err)
	}
	artifacts, err := sdkgen.Generate(model, reference)
	if err != nil {
		return err
	}
	if err := writeFile(*sourcePath, artifacts.Source); err != nil {
		return err
	}
	if err := writeFile(*manifestPath, artifacts.Manifest); err != nil {
		return err
	}
	return nil
}

func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".naatre-swift-*")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write output: %w", err)
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace output: %w", err)
	}
	return nil
}
