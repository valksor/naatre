#![forbid(unsafe_code)]

use naatre_sdk::generator;
use std::env;
use std::error::Error;
use std::fs;
use std::io::{Error as IoError, ErrorKind};
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    let arguments = env::args_os().skip(1).collect::<Vec<_>>();
    if arguments.len() != 3 {
        return Err(IoError::new(
            ErrorKind::InvalidInput,
            "usage: naatre-rust-sdk-generator MODEL REFERENCE OUTPUT_DIRECTORY",
        )
        .into());
    }

    let model = fs::read(&arguments[0])?;
    let reference = fs::read(&arguments[1])?;
    let artifacts = generator::generate(&model, &reference)?;
    let output = PathBuf::from(&arguments[2]);
    fs::create_dir_all(&output)?;
    fs::write(output.join("operations.rs"), artifacts.source)?;
    fs::write(output.join("operations.json"), artifacts.manifest)?;
    Ok(())
}
