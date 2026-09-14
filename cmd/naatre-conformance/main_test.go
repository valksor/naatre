package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestMainReadsEOFAndReturns(t *testing.T) {
	previousArgs := os.Args
	previousFlags := flag.CommandLine
	previousStdin := os.Stdin
	previousStdout := os.Stdout
	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousFlags
		os.Stdin = previousStdin
		os.Stdout = previousStdout
	})

	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open stdin: %v", err)
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open stdout: %v", err)
	}
	defer func() { _ = output.Close() }()

	os.Args = []string{"naatre-conformance", "--suite", filepath.Join("..", "..", "conformance", "v1", "suite.json")}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	os.Stdin = input
	os.Stdout = output
	main()
}
