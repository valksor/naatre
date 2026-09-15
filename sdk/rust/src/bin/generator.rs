use naatre_sdk::generator::generate;
use std::env;
use std::fs;
use std::io::Write as _;
use std::path::{Path, PathBuf};

fn main() {
    if let Err(error) = run() {
        eprintln!("{}", error.code());
        std::process::exit(1);
    }
}

fn run() -> Result<(), naatre_sdk::ClientError> {
    let arguments: Vec<_> = env::args_os().skip(1).collect();
    if arguments.len() != 3 {
        return Err(naatre_sdk::ClientError::new(
            "RUST_SDK_GENERATOR_USAGE",
            "generator requires model, reference, and output directory",
        ));
    }
    let model = fs::read(&arguments[0]).map_err(|_| io_error())?;
    let reference = fs::read(&arguments[1]).map_err(|_| io_error())?;
    let artifacts = generate(&model, &reference)?;
    let output = PathBuf::from(&arguments[2]);
    if fs::symlink_metadata(&output).is_ok_and(|metadata| metadata.file_type().is_symlink()) {
        return Err(path_error());
    }
    fs::create_dir_all(&output).map_err(|_| io_error())?;
    if !fs::metadata(&output).is_ok_and(|metadata| metadata.is_dir()) {
        return Err(path_error());
    }
    atomic_write(&output, "operations.rs", &artifacts.source)?;
    atomic_write(&output, "operations.json", &artifacts.manifest)?;
    Ok(())
}

fn atomic_write(root: &Path, name: &str, contents: &[u8]) -> Result<(), naatre_sdk::ClientError> {
    let destination = root.join(name);
    let temporary = root.join(format!(".{name}.tmp-{}", std::process::id()));
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temporary)
        .map_err(|_| io_error())?;
    if file
        .write_all(contents)
        .and_then(|()| file.sync_all())
        .is_err()
    {
        let _ = fs::remove_file(&temporary);
        return Err(io_error());
    }
    drop(file);
    if let Err(error) = fs::rename(&temporary, &destination) {
        let _ = fs::remove_file(&temporary);
        return Err(if error.kind() == std::io::ErrorKind::PermissionDenied {
            path_error()
        } else {
            io_error()
        });
    }
    Ok(())
}

const fn io_error() -> naatre_sdk::ClientError {
    naatre_sdk::ClientError::new("RUST_SDK_GENERATOR_IO", "generator I/O failed")
}

const fn path_error() -> naatre_sdk::ClientError {
    naatre_sdk::ClientError::new(
        "RUST_SDK_GENERATOR_PATH_ESCAPE",
        "generator output path is not writable",
    )
}
