package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/valksor/naatre/playground"
	"github.com/valksor/naatre/schema"
)

const maximumSchemaBytes = 8 << 20

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "naatre playground failed")
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("naatre-playground", flag.ContinueOnError)
	flags.SetOutput(stderr)
	schemaPath := flags.String("schema", "", "authorized offline schema bundle")
	if flags.Parse(args) != nil || *schemaPath == "" || flags.NArg() != 0 {
		return errors.New("usage")
	}
	document, err := readSchema(*schemaPath)
	if err != nil {
		return err
	}
	handler, err := playground.NewHandler(playground.Config{Schema: document, Limits: playground.DefaultLimits()})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
	}
	_, _ = fmt.Fprintf(stdout, "http://%s\n", listener.Addr())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}

func readSchema(path string) (schema.Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return schema.Document{}, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maximumSchemaBytes+1))
	if err != nil || len(content) > maximumSchemaBytes {
		return schema.Document{}, errors.New("schema input failed")
	}
	return schema.ParseDocument(content, schema.ImportOptions{})
}
