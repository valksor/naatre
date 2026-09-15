//! Command-line entry point for the Naatre Rust SDK generator.
//!
//! Reads the language-neutral generator model and the reference output, then
//! writes the generated operation source and manifest into the target
//! directory. It is a thin wrapper over [`naatre_sdk::generator::generate`] so
//! the generated artifacts stay reproducible.

use std::path::Path;
use std::process::ExitCode;

fn main() -> ExitCode {
    let arguments: Vec<String> = std::env::args().skip(1).collect();
    let [model_path, reference_path, output_directory] = arguments.as_slice() else {
        eprintln!("usage: naatre-rust-sdk-generator <model.json> <reference.json> <output-dir>");
        return ExitCode::FAILURE;
    };

    let model = match std::fs::read(model_path) {
        Ok(bytes) => bytes,
        Err(error) => {
            eprintln!("read model {model_path}: {error}");
            return ExitCode::FAILURE;
        }
    };
    let reference = match std::fs::read(reference_path) {
        Ok(bytes) => bytes,
        Err(error) => {
            eprintln!("read reference {reference_path}: {error}");
            return ExitCode::FAILURE;
        }
    };

    let artifacts = match naatre_sdk::generator::generate(&model, &reference) {
        Ok(artifacts) => artifacts,
        Err(error) => {
            eprintln!("generate rust sdk: {error}");
            return ExitCode::FAILURE;
        }
    };

    let directory = Path::new(output_directory);
    if let Err(error) = std::fs::write(directory.join("operations.rs"), &artifacts.source) {
        eprintln!("write operations.rs: {error}");
        return ExitCode::FAILURE;
    }
    if let Err(error) = std::fs::write(directory.join("operations.json"), &artifacts.manifest) {
        eprintln!("write operations.json: {error}");
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}
