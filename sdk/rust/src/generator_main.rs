use naatre_sdk::generator::generate;
use std::env;
use std::error::Error;
use std::fs;
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    let arguments: Vec<_> = env::args_os().skip(1).collect();
    if arguments.len() != 3 {
        return Err("usage: naatre-rust-sdk-generator MODEL REFERENCE OUTPUT".into());
    }
    let model = fs::read(&arguments[0])?;
    let reference = fs::read(&arguments[1])?;
    let output = PathBuf::from(&arguments[2]);
    let artifacts = generate(&model, &reference)?;
    fs::create_dir_all(&output)?;
    fs::write(output.join("operations.rs"), artifacts.source)?;
    fs::write(output.join("operations.json"), artifacts.manifest)?;
    Ok(())
}
