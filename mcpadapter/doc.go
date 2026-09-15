// Package mcpadapter defines the transport-independent core of Naatre's MCP
// adapter profile. It validates untrusted MCP discovery data against trusted
// application registrations and produces bounded manifests and fidelity
// reports. RuntimeProfile adds bounded Go client/server adapters for stdio and
// Streamable HTTP without becoming another protocol or schema authority.
package mcpadapter
