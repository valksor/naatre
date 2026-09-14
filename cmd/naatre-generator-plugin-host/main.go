package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/valksor/naatre/generatorplugin"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stderr))
}

func run(ctx context.Context, arguments []string, stderr io.Writer) int {
	if len(arguments) != 4 {
		_, _ = fmt.Fprintln(stderr, "usage: naatre-generator-plugin-host MANIFEST RUNTIME MODEL OUTPUT_ROOT")
		return 2
	}
	plugins, err := generatorplugin.Discover([]string{arguments[0]})
	if err != nil || len(plugins) != 1 {
		_, _ = fmt.Fprintln(stderr, "generator plugin discovery failed")
		return 1
	}
	limits := generatorplugin.DefaultLimits()
	model, err := readBounded(arguments[2], limits.ModelBytes)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "generator model could not be read")
		return 1
	}
	host := generatorplugin.Host{Runtimes: map[string]string{plugins[0].Manifest.Runtime: arguments[1]}, Limits: limits}
	if err := host.Generate(ctx, plugins[0], model, nil, arguments[3]); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func readBounded(path string, maximum int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil || len(content) > maximum {
		return nil, fmt.Errorf("model exceeds limit")
	}
	return content, nil
}
