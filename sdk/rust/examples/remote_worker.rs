use serde_json::{Value, json};
use std::io::{self, Read, Write};

const PROTOCOL: &str = "naatre.remote-worker.v1";

fn handle(envelope: &Value) -> Value {
    assert_eq!(envelope["protocol"], PROTOCOL);
    let payload = &envelope["payload"];
    match envelope["kind"].as_str().expect("kind") {
        "register" => {
            json!({"protocol":PROTOCOL,"kind":"registered","payload":{"protocol":PROTOCOL,"workerId":payload["workerId"],"sessionId":"example-session","schemaRevision":payload["schemaRevision"],"acceptedCapabilities":payload["capabilities"]}})
        }
        "cancel" => {
            json!({"protocol":PROTOCOL,"kind":"cancelled","payload":{"protocol":PROTOCOL,"invocationId":payload["invocationId"],"disposition":"acknowledged"}})
        }
        "invoke" => {
            let name = payload["input"]["name"].as_str().expect("input name");
            let rejected = name == "reject";
            json!({"protocol":PROTOCOL,"kind":"result","payload":{"protocol":PROTOCOL,"invocationId":payload["invocationId"],"attemptId":payload["attemptId"],"schemaRevision":payload["schemaRevision"],"data":if rejected { Value::Null } else { json!({"greeting":format!("Hello, {name}")}) },"errors":if rejected { json!([{"code":"NAME_REJECTED","message":"name was rejected","retryable":false}]) } else { json!([]) }}})
        }
        _ => panic!("unsupported envelope"),
    }
}

fn serve(maximum: u32) {
    let mut input = io::stdin().lock();
    let mut output = io::stdout().lock();
    loop {
        let mut header = [0_u8; 5];
        match input.read_exact(&mut header) {
            Ok(()) => {}
            Err(error) if error.kind() == io::ErrorKind::UnexpectedEof => return,
            Err(error) => panic!("read header: {error}"),
        }
        let size = u32::from_be_bytes(header[1..5].try_into().expect("size"));
        assert!(header[0] == 0 && size > 0 && size <= maximum);
        let mut payload = vec![0; size as usize];
        input.read_exact(&mut payload).expect("read frame");
        let encoded = serde_json::to_vec(&handle(&serde_json::from_slice(&payload).expect("JSON")))
            .expect("encode");
        let encoded_size = u32::try_from(encoded.len()).expect("encoded frame fits u32");
        output.write_all(&[0]).expect("write flags");
        output
            .write_all(&encoded_size.to_be_bytes())
            .expect("write size");
        output.write_all(&encoded).expect("write payload");
        output.flush().expect("flush");
    }
}

fn self_test(fixture: &Value) {
    assert_eq!(fixture["schema"]["sharedSchemaProfile"], "core.schema-1");
    for operation in fixture["operations"].as_array().expect("operations") {
        let response = handle(
            &json!({"protocol":PROTOCOL,"kind":"invoke","payload":{"invocationId":operation["name"],"attemptId":"attempt-1","schemaRevision":fixture["schema"]["revision"],"handlerId":operation["handlerId"],"input":operation["input"]}}),
        );
        assert_eq!(
            json!({"data":response["payload"]["data"],"errors":response["payload"]["errors"]}),
            operation["expected"]
        );
    }
}

fn main() {
    let fixture: Value = serde_json::from_slice(include_bytes!(
        "../../../conformance/v1/remote-workers.json"
    ))
    .expect("fixture");
    if std::env::args().any(|argument| argument == "--serve") {
        let maximum = u32::try_from(
            fixture["transport"]["frame"]["maximumBytes"]
                .as_u64()
                .expect("maximum"),
        )
        .expect("frame maximum fits u32");
        serve(maximum);
    } else {
        self_test(&fixture);
    }
}
