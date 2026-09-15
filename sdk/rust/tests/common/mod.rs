use naatre_sdk::{ClientError, Operation, OperationKind};
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Deserialize, PartialEq)]
pub struct Payload {
    pub name: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct Variables {
    pub id: String,
}

pub fn payload_operation() -> Operation<Variables, Payload> {
    Operation::new(
        "Payload",
        OperationKind::Query,
        naatre_sdk::PersistedReference::new("0".repeat(64)).expect("valid test digest"),
        |input| {
            serde_json::from_slice(input)
                .map_err(|_| ClientError::new("CLIENT_RESULT_INVALID", "invalid result"))
        },
    )
}
