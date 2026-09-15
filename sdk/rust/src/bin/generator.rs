use std::{env, fs, path::Path, process::ExitCode};

use naatre_sdk::generator;

fn main() -> ExitCode {
    let arguments = env::args_os().skip(1).collect::<Vec<_>>();
    if arguments.len() != 3 {
        eprintln!("usage: naatre-rust-sdk-generator MODEL REFERENCE OUTPUT_ROOT");
        return ExitCode::from(2);
    }

    let Ok(model) = fs::read(&arguments[0]) else {
        eprintln!("Rust SDK generator model could not be read");
        return ExitCode::FAILURE;
    };
    let Ok(reference) = fs::read(&arguments[1]) else {
        eprintln!("Rust SDK generator reference could not be read");
        return ExitCode::FAILURE;
    };
    let artifacts = match generator::generate(&model, &reference) {
        Ok(artifacts) => artifacts,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };

    let output_root = Path::new(&arguments[2]);
    if fs::create_dir_all(output_root).is_err()
        || fs::write(output_root.join("operations.rs"), artifacts.source).is_err()
        || fs::write(output_root.join("operations.json"), artifacts.manifest).is_err()
    {
        eprintln!("Rust SDK generator output could not be written");
        return ExitCode::FAILURE;
    }

    ExitCode::SUCCESS
}
