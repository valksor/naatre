use naatre_sdk::generator;
use std::env;
use std::fs;
use std::path::Path;
use std::process::ExitCode;

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(code) => {
            eprintln!("{code}");
            ExitCode::FAILURE
        }
    }
}

fn run() -> Result<(), &'static str> {
    let mut arguments = env::args_os().skip(1);
    let model_path = arguments.next().ok_or("RUST_SDK_GENERATOR_ARGUMENTS")?;
    let reference_path = arguments.next().ok_or("RUST_SDK_GENERATOR_ARGUMENTS")?;
    let output_path = arguments.next().ok_or("RUST_SDK_GENERATOR_ARGUMENTS")?;
    if arguments.next().is_some() {
        return Err("RUST_SDK_GENERATOR_ARGUMENTS");
    }

    let model = fs::read(model_path).map_err(|_| "RUST_SDK_GENERATOR_READ")?;
    let reference = fs::read(reference_path).map_err(|_| "RUST_SDK_GENERATOR_READ")?;
    let artifacts = generator::generate(&model, &reference).map_err(|error| error.code())?;
    let output = Path::new(&output_path);
    fs::create_dir_all(output).map_err(|_| "RUST_SDK_GENERATOR_WRITE")?;
    fs::write(output.join("operations.rs"), artifacts.source)
        .map_err(|_| "RUST_SDK_GENERATOR_WRITE")?;
    fs::write(output.join("operations.json"), artifacts.manifest)
        .map_err(|_| "RUST_SDK_GENERATOR_WRITE")?;
    Ok(())
}
