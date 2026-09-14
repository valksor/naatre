package main

import (
	"fmt"
	"io"
	"os"

	"github.com/valksor/naatre/generator"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: naatre-generator MODEL")
		return 2
	}
	input, err := os.ReadFile(arguments[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "generator model could not be read")
		return 1
	}
	output, err := generator.Generate(input)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := stdout.Write(output); err != nil {
		_, _ = fmt.Fprintln(stderr, "generator output could not be written")
		return 1
	}
	return 0
}
