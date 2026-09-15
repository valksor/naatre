use naatre_sdk::generator;
use std::env;
use std::fs;
use std::path::Path;

fn main() {
    if let Err(message) = run() {
        eprintln!("{message}");
        std::process::exit(1);
    }
}

fn run() -> Result<(), &'static str> {
    let mut arguments = env::args_os().skip(1);
    let model_path = arguments
        .next()
        .ok_or("usage: naatre-rust-sdk-generator MODEL REFERENCE OUTPUT_ROOT")?;
    let reference_path = arguments
        .next()
        .ok_or("usage: naatre-rust-sdk-generator MODEL REFERENCE OUTPUT_ROOT")?;
    let output_root = arguments
        .next()
        .ok_or("usage: naatre-rust-sdk-generator MODEL REFERENCE OUTPUT_ROOT")?;
    if arguments.next().is_some() {
        return Err("usage: naatre-rust-sdk-generator MODEL REFERENCE OUTPUT_ROOT");
    }

    let model = fs::read(model_path).map_err(|_| "Rust SDK generator model could not be read")?;
    let reference =
        fs::read(reference_path).map_err(|_| "Rust SDK generator reference could not be read")?;
    let artifacts =
        generator::generate(&model, &reference).map_err(|_| "Rust SDK generation failed")?;
    let output_root = Path::new(&output_root);
    fs::create_dir_all(output_root)
        .map_err(|_| "Rust SDK generator output directory could not be created")?;
    write_artifact(output_root, "operations.rs", &artifacts.source)?;
    write_artifact(output_root, "operations.json", &artifacts.manifest)
}

fn write_artifact(root: &Path, name: &str, content: &[u8]) -> Result<(), &'static str> {
    let destination = root.join(name);
    let temporary = root.join(format!(".{name}.tmp"));
    fs::write(&temporary, content)
        .map_err(|_| "Rust SDK generator artifact could not be written")?;
    fs::rename(&temporary, destination)
        .map_err(|_| "Rust SDK generator artifact could not be installed")
}
