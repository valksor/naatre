//! The environment fallbacks (`NAATRE_S3_*`, `NAATRE_PG_DSN`), pinned in
//! their own process: env mutation is process-global, so this binary runs
//! ONE test — no other parse test's expectations an exported variable
//! could disturb, and no second test racing the same variables.

use naatre_store::backend::{
    BackendError, ENV_PG_DSN, ENV_S3_ACCESS_KEY_ID, ENV_S3_BUCKET, ENV_S3_ENDPOINT, ENV_S3_REGION,
    ENV_S3_SECRET, parse,
};

fn err_of(text: &str) -> BackendError {
    parse(text).unwrap_err()
}

#[test]
fn env_fallbacks_file_wins_and_refusals_hold() {
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

    // The DSN obeys the same precedence as every other key: the FILE wins
    // over `NAATRE_PG_DSN` (env-wins made a file's dsn a lie whenever the
    // variable was set). Env-only still configures (the CI/secret-store
    // injection point), and a dsn from either side is refused under
    // `meta = "sqlite"` unchanged.
    unsafe {
        std::env::set_var(ENV_PG_DSN, "postgres://envhost:1111/envdb?sslmode=require");
    }
    // Env-only configuration parses.
    let cfg = parse("[backend]\nmeta = \"postgres\"\n").unwrap();
    let pg = cfg.pg.unwrap();
    assert_eq!(pg.host, "envhost");
    assert_eq!(pg.port, 1111);
    assert_eq!(pg.dbname, "envdb");
    // The file wins over the environment.
    let cfg = parse(
        "[backend]\nmeta = \"postgres\"\ndsn = \"postgres://filehost:2222/filedb?sslmode=require\"\n",
    )
    .unwrap();
    let pg = cfg.pg.unwrap();
    assert_eq!(pg.host, "filehost");
    assert_eq!(pg.port, 2222);
    assert_eq!(pg.dbname, "filedb");
    // Refused under sqlite unchanged (file key present).
    let e = err_of(
        "[backend]\nmeta = \"sqlite\"\ndsn = \"postgres://filehost:2222/filedb?sslmode=require\"\n",
    );
    assert!(matches!(e, BackendError::KeyWithoutPostgres { ref key } if key == "dsn"));
    unsafe {
        std::env::remove_var(ENV_PG_DSN);
    }
}
