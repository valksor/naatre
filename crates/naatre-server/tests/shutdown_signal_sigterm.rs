//! Install-at-call pin for the SIGTERM arm of
//! [`naatre_server::serve::unix_shutdown_signal`], in its OWN test binary:
//! handlers are process-wide, so one binary per signal means this process
//! starts with DEFAULT dispositions and the raised signal can only be
//! delivered by the contract's own handler — a concurrent sibling pin can
//! never pre-install the other arm for us.
//!
//! Failure mode (true and loud): if the handler were lazy (installed at
//! the future's first poll), `raise(SIGTERM)` below — fired between the
//! call and that first poll — takes the DEFAULT action and this whole
//! test binary dies by SIGTERM at the raise line. There is no softer
//! assertion: the defect IS the process death.
#![cfg(unix)]

use std::time::Duration;

#[tokio::test]
async fn sigterm_raised_before_the_first_poll_is_delivered_on_it() {
    let sig = naatre_server::serve::unix_shutdown_signal();
    // SAFETY: `raise` has no memory-safety preconditions; its only
    // failure mode is a nonzero return, asserted below.
    unsafe {
        assert_eq!(libc::raise(libc::SIGTERM), 0, "raise(SIGTERM)");
    }
    tokio::time::timeout(Duration::from_secs(5), sig)
        .await
        .expect("SIGTERM raised before the first poll must resolve the future");
}
