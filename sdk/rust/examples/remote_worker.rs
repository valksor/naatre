#![cfg(feature = "server")]

use naatre_sdk::{
    HandlerContext, HandlerDescriptor, HandlerFuture, Principal, Registration, RequestOutcome,
    RequestResources, RequestScopeFactory, SchemaValidator, ServerBuilder, WorkerError,
    WorkerInvocation, decode_worker_frame, encode_worker_frame,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::any::Any;
use std::io::{Read, Write};
use std::pin::Pin;
use std::process::ExitCode;
use std::sync::Arc;
use std::task::{Context, Poll, Waker};

const MAXIMUM_FRAME_BYTES: usize = 4096;

#[derive(Deserialize)]
struct GreetInput {
    name: String,
}

#[derive(Serialize)]
struct GreetOutput {
    greeting: String,
}

struct Validator;

impl SchemaValidator for Validator {
    fn validate_input(&self, schema: &str, input: &Value) -> Result<(), WorkerError> {
        if schema == "GreetInput" && input.get("name").and_then(Value::as_str).is_some() {
            Ok(())
        } else {
            Err(WorkerError::new(
                "INPUT_INVALID",
                "invalid greeting input",
                false,
            ))
        }
    }

    fn validate_output(&self, schema: &str, output: &Value) -> Result<(), WorkerError> {
        if schema == "GreetOutput" && output.get("greeting").is_some() {
            Ok(())
        } else {
            Err(WorkerError::new(
                "OUTPUT_COMPLETION",
                "invalid greeting output",
                false,
            ))
        }
    }
}

struct Resources {
    principal: Principal,
    loader: (),
}

impl RequestResources for Resources {
    fn principal(&self) -> &Principal {
        &self.principal
    }

    fn loader(&mut self) -> &mut (dyn Any + Send) {
        &mut self.loader
    }

    fn transaction(&mut self) -> Option<&mut (dyn Any + Send)> {
        None
    }

    fn finish(self: Box<Self>, _outcome: RequestOutcome) {}
}

struct Scopes;

impl RequestScopeFactory for Scopes {
    fn begin(
        &self,
        invocation: &WorkerInvocation,
    ) -> Result<Box<dyn RequestResources>, WorkerError> {
        if invocation.delegated_context != "valid-delegation" {
            return Err(WorkerError::new(
                "UNAUTHORIZED",
                "delegation rejected",
                false,
            ));
        }
        Ok(Box::new(Resources {
            principal: Principal {
                subject: "example-user".to_owned(),
                tenant: "example-tenant".to_owned(),
            },
            loader: (),
        }))
    }
}

fn greet(_context: &mut HandlerContext, input: GreetInput) -> HandlerFuture<'_, GreetOutput> {
    Box::pin(async move {
        if input.name == "reject" {
            Err(WorkerError::new(
                "NAME_REJECTED",
                "name was rejected",
                false,
            ))
        } else {
            Ok(GreetOutput {
                greeting: format!("Hello, {}", input.name),
            })
        }
    })
}

fn read_frame(input: &mut impl Read) -> Result<Vec<u8>, String> {
    let mut header = [0_u8; 5];
    input
        .read_exact(&mut header)
        .map_err(|error| error.to_string())?;
    let size = u32::from_be_bytes([header[1], header[2], header[3], header[4]]) as usize;
    if header[0] != 0 || size == 0 || size > MAXIMUM_FRAME_BYTES {
        return Err("invalid frame header".to_owned());
    }
    let mut frame = Vec::with_capacity(size + 5);
    frame.extend_from_slice(&header);
    frame.resize(size + 5, 0);
    input
        .read_exact(&mut frame[5..])
        .map_err(|error| error.to_string())?;
    Ok(frame)
}

fn write_frame(output: &mut impl Write, frame: &[u8]) -> Result<(), String> {
    output.write_all(frame).map_err(|error| error.to_string())?;
    output.flush().map_err(|error| error.to_string())
}

fn run() -> Result<(), String> {
    let expected: Registration = serde_json::from_slice(include_bytes!(
        "../../../conformance/v1/remote-workers.json"
    ))
    .and_then(|fixture: Value| serde_json::from_value(fixture["registration"].clone()))
    .map_err(|error| error.to_string())?;
    let mut builder = ServerBuilder::new(expected.clone(), Arc::new(Validator), Arc::new(Scopes))
        .map_err(|error| error.to_string())?;
    builder
        .register_handler(
            HandlerDescriptor::unary("fixture.greet", "GreetInput", "GreetOutput", "query"),
            (),
            |(), context, input| greet(context, input),
        )
        .map_err(|error| error.to_string())?;
    let worker = builder.build().map_err(|error| error.to_string())?;

    let mut input = std::io::stdin().lock();
    let mut output = std::io::stdout().lock();
    let registration: Registration =
        decode_worker_frame(&read_frame(&mut input)?, MAXIMUM_FRAME_BYTES)
            .map_err(|error| error.to_string())?;
    if registration != expected {
        return Err("gateway registration does not match worker manifest".to_owned());
    }
    write_frame(
        &mut output,
        &encode_worker_frame(
            &worker.registration_ack("example-session"),
            MAXIMUM_FRAME_BYTES,
        )
        .map_err(|error| error.to_string())?,
    )?;

    loop {
        let frame = match read_frame(&mut input) {
            Ok(frame) => frame,
            Err(error) if error.contains("failed to fill whole buffer") => return Ok(()),
            Err(error) => return Err(error),
        };
        let invocation: WorkerInvocation =
            decode_worker_frame(&frame, MAXIMUM_FRAME_BYTES).map_err(|error| error.to_string())?;
        let mut future = worker
            .invoke(invocation)
            .map_err(|error| error.to_string())?;
        let mut context = Context::from_waker(Waker::noop());
        let Poll::Ready(result) = Pin::new(&mut future).poll(&mut context) else {
            return Err("example handler requires an executor".to_owned());
        };
        let result = result.map_err(|error| error.to_string())?;
        write_frame(
            &mut output,
            &encode_worker_frame(&result, MAXIMUM_FRAME_BYTES)
                .map_err(|error| error.to_string())?,
        )?;
    }
}

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("{error}");
            ExitCode::FAILURE
        }
    }
}
