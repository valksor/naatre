package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/valksor/naatre/internal/conformancerunner"
)

func main() {
	suite := flag.String("suite", "conformance/v1/suite.json", "path to the conformance suite manifest")
	listen := flag.String("http", "", "serve the HTTP runner binding at this address")
	requirePass := flag.Bool("require-pass", false, "exit non-zero unless every requested profile passes")
	flag.Parse()

	runner, err := conformancerunner.New(*suite)
	if err != nil {
		fail(err)
	}
	if *listen == "" {
		options := conformancerunner.NDJSONOptions{RequirePass: *requirePass}
		if err := runner.ServeNDJSONWithOptions(context.Background(), os.Stdin, os.Stdout, options); err != nil {
			fail(err)
		}
		return
	}
	if *requirePass {
		fail(fmt.Errorf("--require-pass is only valid for the NDJSON binding"))
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           runner.HTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
