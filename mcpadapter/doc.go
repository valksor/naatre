// Package mcpadapter defines the transport-independent core of Naatre's MCP
// adapter profile. It validates untrusted MCP discovery data against trusted
// application registrations and produces bounded manifests and fidelity
// reports. Concrete MCP clients, servers, and wire transports belong to the
// separately versioned integration profile.
package mcpadapter
