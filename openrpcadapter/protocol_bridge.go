package openrpcadapter

import "github.com/valksor/naatre/protocol"

func protocolValidateJSON(raw []byte, limits Limits) error {
	return protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: int(limits.MaxResponseBytes), MaxDepth: limits.MaxDepth})
}

func protocolValidateRequestJSON(raw []byte, limits Limits) error {
	return protocol.ValidateJSON(raw, protocol.Limits{MaxBytes: int(limits.MaxRequestBytes), MaxDepth: limits.MaxDepth})
}
