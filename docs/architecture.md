# Architecture and dependency direction

`schema` and `protocol` are portable public contracts. `runtime` depends on
them to register and execute operations. `transport/http` adapts the runtime to
HTTP. SDKs consume protocol and schema documents but do not import runtime
implementation details. Executable `examples` may assemble public packages but
are not imported by the implementation. `internal/conformance` may assemble all
components for testing and cannot be imported by external modules.

No package may expose application values or methods through implicit
reflection. Registration is explicit. Decode, validate, plan, authorize,
execute, complete, and serialize remain distinct phases, and no business
handler runs before complete validation.
