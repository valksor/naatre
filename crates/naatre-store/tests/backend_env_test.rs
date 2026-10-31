//! The NAATRE_S3_* environment fallback, pinned in its own process: env
//! mutation is process-global, and this binary runs no other parse tests
//! whose expectations an exported variable could disturb.

use naatre_store::backend::{
    BackendError, ENV_S3_ACCESS_KEY_ID, ENV_S3_BUCKET, ENV_S3_ENDPOINT, ENV_S3_REGION,
    ENV_S3_SECRET, parse,
};

fn err_of(text: &str) -> BackendError {
    parse(text).unwrap_err()
}

#[test]
fn s3_keys_via_environment_fall_back_file_wins_and_refuse_under_fs() {
    unsafe {
        std::env::set_var(ENV_S3_BUCKET, "naatre-test");
        std::env::set_var(ENV_S3_ACCESS_KEY_ID, "envkey");
        std::env::set_var(ENV_S3_SECRET, "envsecret");
    }
    // Env-only configuration parses.
    let cfg = parse("[backend]\nblob = \"s3\"\n").unwrap();
    let s3 = cfg.s3.unwrap();
    assert_eq!(s3.bucket, "naatre-test");
    assert_eq!(s3.access_key_id, "envkey");
    assert_eq!(s3.secret, "envsecret");
    // Endpoint and region ride along from their own variables.
    unsafe {
        std::env::set_var(ENV_S3_ENDPOINT, "127.0.0.1:9000");
        std::env::set_var(ENV_S3_REGION, "us-east-1");
    }
    let cfg = parse("[backend]\nblob = \"s3\"\n").unwrap();
    let s3 = cfg.s3.unwrap();
    assert_eq!(s3.endpoint.as_deref(), Some("127.0.0.1:9000"));
    assert_eq!(s3.region.as_deref(), Some("us-east-1"));
    // The file wins over the environment.
    let cfg = parse("[backend]\nblob = \"s3\"\nbucket = \"from-file\"\n").unwrap();
    assert_eq!(cfg.s3.unwrap().bucket, "from-file");
    // And the env-only keys are refused under blob = fs, same as file keys
    // (the guard checks endpoint first).
    let e = err_of("[backend]\nblob = \"fs\"\n");
    assert!(matches!(e, BackendError::KeyWithoutBlobS3 { key: ref k } if k == "endpoint"));
    unsafe {
        std::env::remove_var(ENV_S3_BUCKET);
        std::env::remove_var(ENV_S3_ACCESS_KEY_ID);
        std::env::remove_var(ENV_S3_SECRET);
        std::env::remove_var(ENV_S3_ENDPOINT);
        std::env::remove_var(ENV_S3_REGION);
    }
}
