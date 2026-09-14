// Package swift binds reproducible generated artifacts to go generate.
package swift

//go:generate go run ../../cmd/naatre-swift-sdk-generator -model ../../conformance/v1/generator-model.json -reference ../../conformance/v1/generator-output.json -source Sources/NaatreGenerated/Operations.swift -manifest Sources/NaatreGenerated/operations.json
