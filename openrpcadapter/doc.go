// Package openrpcadapter implements the optional adapter.openrpc-jsonrpc-1
// profile. It maps a strict, bounded OpenRPC 1.4.1 subset and adapts JSON-RPC
// 2.0 calls in both runtime-consume and runtime-expose directions.
//
// The package builds on interopadapter's protocol-neutral fidelity contract.
// OpenRPC method names and description annotations are untrusted input: every
// exposed or consumed method requires an application-supplied runtime
// descriptor and an explicit allowlist entry.
package openrpcadapter

const (
	Profile              = "adapter.openrpc-jsonrpc-1"
	OpenRPCVersion       = "1.4.1"
	JSONRPCVersion       = "2.0"
	Specification        = "https://github.com/open-rpc/spec/releases/tag/v1.4.1"
	JSONRPCSpecification = "https://www.jsonrpc.org/specification"
)
