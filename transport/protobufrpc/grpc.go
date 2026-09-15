package protobufrpc

import (
	"context"
	"errors"
	"io"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

func (a *Adapter) invokeGRPC(ctx context.Context, input proto.Message) (proto.Message, error) {
	outgoing, _, err := a.outgoingMetadata(ctx)
	if err != nil {
		return nil, err
	}
	output := dynamicpb.NewMessage(a.method.Output())
	var headers, trailers metadata.MD
	if err := a.grpc.Invoke(outgoing, fullMethod(a.method), input, output, grpc.Header(&headers), grpc.Trailer(&trailers)); err != nil {
		return nil, classifyGRPCError(ctx, err)
	}
	if len(headers) != 0 || len(trailers) != 0 {
		return nil, publicError("PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED", string(CodeDataLoss))
	}
	if proto.Size(output) > a.limits.MaxResponseBytes {
		return nil, publicError("PROTO_RPC_RESPONSE_LIMIT", string(CodeResourceExhausted))
	}
	return output, nil
}

func (a *Adapter) openGRPCStream(ctx context.Context, input proto.Message) (*Stream, error) {
	streamContext, cancel := context.WithCancel(ctx)
	outgoing, _, err := a.outgoingMetadata(streamContext)
	if err != nil {
		cancel()
		return nil, err
	}
	description := &grpc.StreamDesc{ServerStreams: true}
	client, err := a.grpc.NewStream(outgoing, description, fullMethod(a.method))
	if err != nil {
		cancel()
		return nil, classifyGRPCError(ctx, err)
	}
	if err := client.SendMsg(input); err != nil {
		cancel()
		_ = client.CloseSend()
		return nil, classifyGRPCError(ctx, err)
	}
	if err := client.CloseSend(); err != nil {
		cancel()
		return nil, classifyGRPCError(ctx, err)
	}
	var headerOnce sync.Once
	var headerErr error
	return &Stream{
		limits: a.limits,
		recv: func() (proto.Message, error) {
			headerOnce.Do(func() {
				headers, err := client.Header()
				if err != nil {
					headerErr = classifyGRPCError(ctx, err)
				} else if len(headers) != 0 {
					headerErr = publicError("PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED", string(CodeDataLoss))
				}
			})
			if headerErr != nil {
				return nil, headerErr
			}
			message := dynamicpb.NewMessage(a.method.Output())
			if err := client.RecvMsg(message); err != nil {
				if errors.Is(err, io.EOF) && len(client.Trailer()) != 0 {
					return nil, publicError("PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED", string(CodeDataLoss))
				}
				return nil, grpcStreamError(ctx, err)
			}
			return message, nil
		},
		close: func() error {
			cancel()
			return client.CloseSend()
		},
	}, nil
}
