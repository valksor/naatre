use naatre_sdk::generator::generate;
use std::env;
use std::ffi::OsString;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

fn main() -> ExitCode {
    let arguments: Vec<_> = env::args_os().skip(1).collect();
    match run(&arguments) {
        Ok(()) => ExitCode::SUCCESS,
        Err(code) => {
            eprintln!("{code}");
            ExitCode::FAILURE
        }
    }
}

fn run(arguments: &[OsString]) -> Result<(), &'static str> {
    let [model_path, reference_path, output_path] = arguments else {
        return Err("RUST_SDK_GENERATOR_USAGE");
    };
    let model = fs::read(model_path).map_err(|_| "RUST_SDK_GENERATOR_INPUT_FAILED")?;
    let reference = fs::read(reference_path).map_err(|_| "RUST_SDK_GENERATOR_INPUT_FAILED")?;
    let artifacts = generate(&model, &reference).map_err(|error| error.code())?;
    let output = PathBuf::from(output_path);
    fs::create_dir_all(&output).map_err(|_| "RUST_SDK_GENERATOR_OUTPUT_FAILED")?;
    write_artifact(&output.join("operations.rs"), &artifacts.source)?;
    write_artifact(&output.join("operations.json"), &artifacts.manifest)
}

fn write_artifact(path: &Path, content: &[u8]) -> Result<(), &'static str> {
    let temporary = path.with_extension("tmp");
    fs::write(&temporary, content).map_err(|_| "RUST_SDK_GENERATOR_OUTPUT_FAILED")?;
    fs::rename(&temporary, path).map_err(|_| {
        let _ = fs::remove_file(&temporary);
        "RUST_SDK_GENERATOR_OUTPUT_FAILED"
    })
}
