package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/valksor/naatre/internal/doccheck"
)

func main() {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		log.Fatal(err)
	}
	codes, err := doccheck.PublicCodes(root)
	if err != nil {
		log.Fatal(err)
	}
	path := filepath.Join(root, "docs", "v1", "error-codes.md")
	if err := os.WriteFile(path, doccheck.RenderErrorIndex(codes), 0o644); err != nil {
		log.Fatal(err)
	}
}
