package protobufrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valksor/naatre/interopadapter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestGRPCUnaryPreservesShapeProjectionMetadataAndDeadline(t *testing.T) {
	t.Parallel()
	method := testMethod(t, false, false, false)
	response := testResponse(method, "18446744073709551615", "Ada")
	var gotInput map[string]any
	var gotMetadata metadata.MD
	connection := &fakeConnection{invoke: func(ctx context.Context, input, output any) error {
		gotMetadata, _ = metadata.FromOutgoingContext(ctx)
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("gRPC invocation omitted caller deadline")
		}
		raw, _ := protojson.Marshal(input.(proto.Message))
		_ = json.Unmarshal(raw, &gotInput)
		proto.Merge(output.(proto.Message), response)
		return nil
	}}
	adapter, report, err := NewGRPC(testConfig(method), connection)
	if err != nil || report.Status != "ready" || report.Streaming != "unary" {
		t.Fatalf("NewGRPC = %#v, %v", report, err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	defer cancel()
	result, err := adapter.Invoke(ctx, interopadapter.BackendRequest{
		Input:      json.RawMessage(`{"id":"-9223372036854775808","email":"ada@example.test"}`),
		Projection: []string{"count", "name"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result["count"] != "18446744073709551615" || result["count32"] != "4294967295" || result["name"] != "Ada" {
		t.Fatalf("result = %#v", result)
	}
	if gotInput["id"] != "-9223372036854775808" || gotInput["email"] != "ada@example.test" || gotInput["phone"] != nil {
		t.Fatalf("protobuf presence, oneof, or numeric range changed: %#v", gotInput)
	}
	mask, ok := gotInput["mask"].(string)
	if !ok || mask != "count,name" {
		t.Fatalf("field mask = %#v", gotInput["mask"])
	}
	if !reflect.DeepEqual(gotMetadata.Get("authorization"), []string{"Bearer credential"}) || len(gotMetadata.Get("cookie")) != 0 {
		t.Fatalf("forwarded metadata = %#v", gotMetadata)
	}
	encoded, err := MarshalReport(report)
	if err != nil || !json.Valid(encoded) || !bytes.Equal(encoded, mustReport(t, report)) {
		t.Fatalf("report = %s, %v", encoded, err)
	}
	if !hasMapping(report, "request.email", "contact", "String", "explicit") {
		t.Fatalf("oneof mapping absent: %#v", report.Mappings)
	}
}

func TestConnectUnaryUsesPinnedWireAndRejectsProtectedMetadata(t *testing.T) {
	t.Parallel()
	method := testMethod(t, false, false, false)
	responseBytes, _ := proto.Marshal(testResponse(method, "42", "Ada"))
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Content-Type") != connectUnaryMediaType || request.Header.Get("Connect-Protocol-Version") != "1" || request.Header.Get("Authorization") != "Bearer credential" || request.Header.Get("Cookie") != "" || request.Header.Get("Connect-Timeout-Ms") == "" {
			t.Fatalf("Connect headers = %#v", request.Header)
		}
		payload, _ := io.ReadAll(request.Body)
		input := dynamicpb.NewMessage(method.Input())
		if err := proto.Unmarshal(payload, input); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		mask := input.Get(method.Input().Fields().ByName("mask")).Message().Get(fieldmaskpb.File_google_protobuf_field_mask_proto.Messages().ByName("FieldMask").Fields().ByName("paths")).List()
		if mask.Len() != 1 || mask.Get(0).String() != "name" {
			t.Fatalf("mask = %#v", mask)
		}
		return protoResponse(http.StatusOK, connectUnaryMediaType, responseBytes), nil
	})
	config := testConfig(method)
	config.HTTPClient = &http.Client{Transport: transport}
	config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
	adapter, report, err := NewConnect(config)
	if err != nil || report.Dependencies["connectProtocol"] != ConnectProtocolRevision {
		t.Fatalf("NewConnect = %#v, %v", report, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := adapter.Invoke(ctx, interopadapter.BackendRequest{Input: json.RawMessage(`{"id":"7"}`), Projection: []string{"name"}})
	if err != nil || result["count"] != "42" {
		t.Fatalf("Invoke = %#v, %v", result, err)
	}
}

func TestGRPCAndConnectMapSafeStableFailuresWithoutSecrets(t *testing.T) {
	t.Parallel()
	method := testMethod(t, false, false, false)
	grpcAdapter, _, err := NewGRPC(testConfig(method), &fakeConnection{invoke: func(context.Context, any, any) error {
		return status.Error(codes.PermissionDenied, "credential-secret internal detail")
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, grpcErr := grpcAdapter.Invoke(context.Background(), interopadapter.BackendRequest{})
	assertPublicError(t, grpcErr, "PROTO_RPC_STATUS", CodePermissionDenied)
	grpcDetailStatus, detailStatusErr := status.New(codes.InvalidArgument, "secret").WithDetails(&anypb.Any{TypeUrl: "credential-secret"})
	if detailStatusErr != nil {
		t.Fatal(detailStatusErr)
	}
	grpcDetailAdapter, _, _ := NewGRPC(testConfig(method), &fakeConnection{invoke: func(context.Context, any, any) error {
		return grpcDetailStatus.Err()
	}})
	_, grpcDetailErr := grpcDetailAdapter.Invoke(context.Background(), interopadapter.BackendRequest{})
	assertPublicError(t, grpcDetailErr, "PROTO_RPC_STATUS_DETAILS_UNSUPPORTED", CodeInvalidArgument)

	grpcMetadataAdapter, _, _ := NewGRPC(testConfig(method), &fakeConnection{
		responseMetadata: metadata.Pairs("set-cookie", "credential-secret"),
		invoke: func(_ context.Context, _, output any) error {
			proto.Merge(output.(proto.Message), testResponse(method, "1", "Ada"))
			return nil
		},
	})
	_, grpcMetadataErr := grpcMetadataAdapter.Invoke(context.Background(), interopadapter.BackendRequest{})
	assertPublicError(t, grpcMetadataErr, "PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED", CodeDataLoss)

	config := testConfig(method)
	config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return protoResponse(http.StatusForbidden, "application/json", []byte(`{"code":"permission_denied","message":"credential-secret internal detail"}`)), nil
	})}
	connectAdapter, _, err := NewConnect(config)
	if err != nil {
		t.Fatal(err)
	}
	_, connectErr := connectAdapter.Invoke(context.Background(), interopadapter.BackendRequest{})
	assertPublicError(t, connectErr, "PROTO_RPC_STATUS", CodePermissionDenied)

	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return protoResponse(http.StatusBadRequest, "application/json", []byte(`{"code":"invalid_argument","message":"secret","details":[{"type":"private"}]}`)), nil
	})}
	detailAdapter, _, _ := NewConnect(config)
	_, detailErr := detailAdapter.Invoke(context.Background(), interopadapter.BackendRequest{})
	assertPublicError(t, detailErr, "PROTO_RPC_STATUS_DETAILS_UNSUPPORTED", CodeInvalidArgument)
}

func TestGRPCServerStreamPreservesMessagesCancellationAndLimits(t *testing.T) {
	t.Parallel()
	method := testMethod(t, true, false, false)
	responses := []proto.Message{testResponse(method, "1", "Ada"), testResponse(method, "2", "Lin")}
	fake := &fakeClientStream{ctx: context.Background(), responses: responses}
	adapter, report, err := NewGRPC(testConfig(method), &fakeConnection{stream: fake})
	if err != nil || report.Streaming != "server" {
		t.Fatalf("NewGRPC = %#v, %v", report, err)
	}
	stream, err := adapter.OpenStream(context.Background(), interopadapter.BackendRequest{Input: json.RawMessage(`{"id":"7"}`)})
	if err != nil {
		t.Fatal(err)
	}
	first, firstErr := stream.Recv()
	second, secondErr := stream.Recv()
	_, eof := stream.Recv()
	if firstErr != nil || secondErr != nil || !errors.Is(eof, io.EOF) || first["count"] != "1" || second["count"] != "2" || !fake.closed {
		t.Fatalf("stream = %#v/%v %#v/%v eof=%v closed=%v", first, firstErr, second, secondErr, eof, fake.closed)
	}
	closeFake := &fakeClientStream{responses: responses}
	closeAdapter, _, _ := NewGRPC(testConfig(method), &fakeConnection{stream: closeFake})
	closeStream, _ := closeAdapter.OpenStream(context.Background(), interopadapter.BackendRequest{})
	if err := closeStream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeFake.ctx.Done():
	default:
		t.Fatal("closing gRPC stream did not cancel its RPC context")
	}

	limitedConfig := testConfig(method)
	limitedConfig.Limits.MaxStreamMessages = 1
	limitedConfig.Limits.MaxStreamBytes = 4096
	limited, _, err := NewGRPC(limitedConfig, &fakeConnection{stream: &fakeClientStream{ctx: context.Background(), responses: responses}})
	if err != nil {
		t.Fatal(err)
	}
	limitedStream, _ := limited.OpenStream(context.Background(), interopadapter.BackendRequest{})
	_, _ = limitedStream.Recv()
	_, limitErr := limitedStream.Recv()
	assertPublicError(t, limitErr, "PROTO_RPC_STREAM_LIMIT", CodeResourceExhausted)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelAdapter, _, _ := NewGRPC(testConfig(method), &fakeConnection{newStreamErr: status.Error(codes.Canceled, "secret")})
	_, cancelErr := cancelAdapter.OpenStream(cancelled, interopadapter.BackendRequest{})
	assertPublicError(t, cancelErr, "PROTO_RPC_CANCELLED", CodeCanceled)
}

func TestConnectServerStreamFramesMessagesAndTerminalStatus(t *testing.T) {
	t.Parallel()
	method := testMethod(t, true, false, false)
	message, _ := proto.Marshal(testResponse(method, "9", "Ada"))
	payload := append(frame(0, message), frame(connectEndStreamFlag, []byte(`{}`))...)
	config := testConfig(method)
	config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestBytes, _ := io.ReadAll(request.Body)
		if request.Header.Get("Content-Type") != connectStreamMediaType || len(requestBytes) < 5 || requestBytes[0] != 0 {
			t.Fatalf("stream request = %x / %#v", requestBytes, request.Header)
		}
		return protoResponse(http.StatusOK, connectStreamMediaType, payload), nil
	})}
	adapter, _, err := NewConnect(config)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := adapter.OpenStream(context.Background(), interopadapter.BackendRequest{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Recv()
	_, eof := stream.Recv()
	if err != nil || result["count"] != "9" || !errors.Is(eof, io.EOF) {
		t.Fatalf("stream = %#v, %v, %v", result, err, eof)
	}
}

func TestConnectCancellationActivelyAbortsRequest(t *testing.T) {
	t.Parallel()
	method := testMethod(t, false, false, false)
	started := make(chan struct{})
	config := testConfig(method)
	config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	adapter, _, err := NewConnect(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, invokeErr := adapter.Invoke(ctx, interopadapter.BackendRequest{})
		done <- invokeErr
	}()
	<-started
	cancel()
	assertPublicError(t, <-done, "PROTO_RPC_CANCELLED", CodeCanceled)
}

func TestConnectRejectsResponseMetadataAndCompression(t *testing.T) {
	t.Parallel()
	unaryMethod := testMethod(t, false, false, false)
	unaryPayload, _ := proto.Marshal(testResponse(unaryMethod, "1", "Ada"))
	for _, test := range []struct {
		name   string
		mutate func(*http.Response)
		code   string
	}{
		{
			name: "application header",
			mutate: func(response *http.Response) {
				response.Header.Set("X-Protected-Metadata", "credential-secret")
			},
			code: "PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED",
		},
		{
			name: "HTTP trailer",
			mutate: func(response *http.Response) {
				response.Trailer = http.Header{"X-Protected-Metadata": {"credential-secret"}}
			},
			code: "PROTO_RPC_TRAILER_METADATA_UNSUPPORTED",
		},
		{
			name: "response compression",
			mutate: func(response *http.Response) {
				response.Header.Set("Connect-Content-Encoding", "gzip")
			},
			code: "PROTO_RPC_COMPRESSION_UNSUPPORTED",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(unaryMethod)
			config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
			config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				response := protoResponse(http.StatusOK, connectUnaryMediaType, unaryPayload)
				test.mutate(response)
				return response, nil
			})}
			adapter, _, err := NewConnect(config)
			if err != nil {
				t.Fatal(err)
			}
			_, invokeErr := adapter.Invoke(context.Background(), interopadapter.BackendRequest{})
			assertPublicError(t, invokeErr, test.code, map[string]RPCCode{
				"PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED": CodeDataLoss,
				"PROTO_RPC_TRAILER_METADATA_UNSUPPORTED":  CodeDataLoss,
				"PROTO_RPC_COMPRESSION_UNSUPPORTED":       CodeUnimplemented,
			}[test.code])
		})
	}

	streamMethod := testMethod(t, true, false, false)
	message, _ := proto.Marshal(testResponse(streamMethod, "2", "Lin"))
	t.Run("stream initial metadata", func(t *testing.T) {
		config := testConfig(streamMethod)
		config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
		config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			response := protoResponse(http.StatusOK, connectStreamMediaType, append(frame(0, message), frame(connectEndStreamFlag, []byte(`{}`))...))
			response.Header.Set("X-Protected-Metadata", "credential-secret")
			return response, nil
		})}
		adapter, _, _ := NewConnect(config)
		_, streamErr := adapter.OpenStream(context.Background(), interopadapter.BackendRequest{})
		assertPublicError(t, streamErr, "PROTO_RPC_RESPONSE_METADATA_UNSUPPORTED", CodeDataLoss)
	})
	t.Run("stream HTTP trailer", func(t *testing.T) {
		trailers := http.Header{}
		payload := append(frame(0, message), frame(connectEndStreamFlag, []byte(`{}`))...)
		config := testConfig(streamMethod)
		config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
		config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			response := protoResponse(http.StatusOK, connectStreamMediaType, nil)
			response.Trailer = trailers
			response.Body = io.NopCloser(&trailerReader{reader: bytes.NewReader(payload), trailers: trailers})
			return response, nil
		})}
		adapter, _, _ := NewConnect(config)
		stream, _ := adapter.OpenStream(context.Background(), interopadapter.BackendRequest{})
		_, _ = stream.Recv()
		_, trailerErr := stream.Recv()
		assertPublicError(t, trailerErr, "PROTO_RPC_TRAILER_METADATA_UNSUPPORTED", CodeDataLoss)
	})
	t.Run("stream end metadata", func(t *testing.T) {
		config := testConfig(streamMethod)
		config.ConnectURL = "https://rpc.example/fixture.v1.Profiles/Lookup"
		config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			payload := append(frame(0, message), frame(connectEndStreamFlag, []byte(`{"metadata":{"x-protected-metadata":["credential-secret"]}}`))...)
			return protoResponse(http.StatusOK, connectStreamMediaType, payload), nil
		})}
		adapter, _, _ := NewConnect(config)
		stream, _ := adapter.OpenStream(context.Background(), interopadapter.BackendRequest{})
		_, _ = stream.Recv()
		_, metadataErr := stream.Recv()
		assertPublicError(t, metadataErr, "PROTO_RPC_TRAILER_METADATA_UNSUPPORTED", CodeUnknown)
	})
}

func TestConfigurationRejectsLossyShapesStreamingAndLimits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		method protoreflect.MethodDescriptor
		code   string
	}{
		{name: "implicit presence", method: testMethod(t, false, true, false), code: "PROTO_RPC_PRESENCE_UNSUPPORTED"},
		{name: "bytes", method: testMethod(t, false, false, true), code: "PROTO_RPC_BYTES_UNSUPPORTED"},
		{name: "client stream", method: testClientStreamMethod(t), code: "PROTO_RPC_STREAMING_UNSUPPORTED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, report, err := NewGRPC(testConfig(test.method), &fakeConnection{})
			if errorCode(err) != test.code || report.Status != "rejected" || !hasDiagnostic(report, test.code) {
				t.Fatalf("NewGRPC = %#v, %v", report, err)
			}
		})
	}
	config := testConfig(testMethod(t, false, false, false))
	config.ForwardMetadata = []string{"Authorization"}
	_, report, err := NewGRPC(config, &fakeConnection{})
	if errorCode(err) != "PROTO_RPC_METADATA_POLICY_INVALID" || !hasDiagnostic(report, "PROTO_RPC_METADATA_POLICY_INVALID") {
		t.Fatalf("metadata policy = %#v, %v", report, err)
	}
	config = testConfig(testMethod(t, false, false, false))
	config.Limits.MaxResponseBytes = 0
	_, report, err = NewGRPC(config, &fakeConnection{})
	if errorCode(err) != "PROTO_RPC_LIMIT_INVALID" || !hasDiagnostic(report, "PROTO_RPC_LIMIT_INVALID") {
		t.Fatalf("limits = %#v, %v", report, err)
	}
}

func TestRequestRejectsUnknownFieldsInvalidRangesAndMetadataLimits(t *testing.T) {
	t.Parallel()
	method := testMethod(t, false, false, false)
	config := testConfig(method)
	config.Limits.MaxMetadataItems = 1
	config.Metadata = func(context.Context) map[string][]string { return map[string][]string{"authorization": {"a", "b"}} }
	adapter, _, err := NewGRPC(config, &fakeConnection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`{"unknown":1}`, `{"id":"9223372036854775808"}`, `{"email":"a","phone":"b"}`} {
		_, invokeErr := adapter.Invoke(context.Background(), interopadapter.BackendRequest{Input: json.RawMessage(input)})
		if errorCode(invokeErr) != "PROTO_RPC_SHAPE_INVALID" {
			t.Fatalf("input %s error = %v", input, invokeErr)
		}
	}
	_, metadataErr := adapter.Invoke(context.Background(), interopadapter.BackendRequest{})
	assertPublicError(t, metadataErr, "PROTO_RPC_METADATA_LIMIT", CodeResourceExhausted)

	config = testConfig(testMethodWithoutFieldMask(t))
	config.FieldMask = ""
	withoutMask, _, _ := NewGRPC(config, &fakeConnection{})
	_, fieldMaskErr := withoutMask.Invoke(context.Background(), interopadapter.BackendRequest{Projection: []string{"name"}})
	assertPublicError(t, fieldMaskErr, "PROTO_RPC_FIELD_MASK_REQUIRED", CodeInvalidArgument)

	adapter, _, _ = NewGRPC(testConfig(method), &fakeConnection{})
	_, conflictErr := adapter.Invoke(context.Background(), interopadapter.BackendRequest{Input: json.RawMessage(`{"mask":"name"}`), Projection: []string{"name"}})
	assertPublicError(t, conflictErr, "PROTO_RPC_FIELD_MASK_CONFLICT", CodeInvalidArgument)
	_, pathErr := adapter.Invoke(context.Background(), interopadapter.BackendRequest{Projection: []string{"missing"}})
	assertPublicError(t, pathErr, "PROTO_RPC_FIELD_MASK_INVALID", CodeInvalidArgument)

	connectConfig := testConfig(method)
	connectConfig.ConnectURL = "https://rpc.example/wrong.Service/Wrong"
	_, endpointReport, endpointErr := NewConnect(connectConfig)
	if errorCode(endpointErr) != "PROTO_RPC_ENDPOINT_INVALID" || !hasDiagnostic(endpointReport, "PROTO_RPC_ENDPOINT_INVALID") {
		t.Fatalf("endpoint mismatch = %#v, %v", endpointReport, endpointErr)
	}
}

func testConfig(method protoreflect.MethodDescriptor) Config {
	return Config{
		Method: method, FieldMask: "mask", ForwardMetadata: []string{"authorization", "x-trace"},
		Metadata: func(context.Context) map[string][]string {
			return map[string][]string{"authorization": {"Bearer credential"}, "cookie": {"protected"}, "x-trace": {"trace-1"}}
		},
		Limits: Limits{MaxRequestBytes: 4096, MaxResponseBytes: 4096, MaxMetadataBytes: 1024, MaxMetadataItems: 8, MaxStreamMessages: 8, MaxStreamBytes: 4096},
	}
}

func testMethod(t testing.TB, serverStream, implicitPresence, bytesField bool) protoreflect.MethodDescriptor {
	t.Helper()
	syntax := "proto2"
	if implicitPresence {
		syntax = "proto3"
	}
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	requestFields := []*descriptorpb.FieldDescriptorProto{
		{Name: proto.String("id"), Number: proto.Int32(1), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
		{Name: proto.String("email"), Number: proto.Int32(2), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), OneofIndex: proto.Int32(0)},
		{Name: proto.String("phone"), Number: proto.Int32(3), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), OneofIndex: proto.Int32(0)},
		{Name: proto.String("mask"), Number: proto.Int32(4), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".google.protobuf.FieldMask")},
	}
	if bytesField {
		requestFields = append(requestFields, &descriptorpb.FieldDescriptorProto{Name: proto.String("blob"), Number: proto.Int32(5), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()})
	}
	file := &descriptorpb.FileDescriptorProto{
		Name: proto.String("fixture.proto"), Package: proto.String("fixture.v1"), Syntax: proto.String(syntax),
		Dependency: []string{"google/protobuf/field_mask.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("LookupRequest"), Field: requestFields, OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("contact")}}},
			{Name: proto.String("LookupResponse"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("count"), Number: proto.Int32(1), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum()},
				{Name: proto.String("name"), Number: proto.Int32(2), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("count32"), Number: proto.Int32(3), Label: &optional, Type: descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum()},
			}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Profiles"), Method: []*descriptorpb.MethodDescriptorProto{{
			Name: proto.String("Lookup"), InputType: proto.String(".fixture.v1.LookupRequest"), OutputType: proto.String(".fixture.v1.LookupResponse"), ServerStreaming: proto.Bool(serverStream),
		}}}},
	}
	descriptor, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	return descriptor.Services().Get(0).Methods().Get(0)
}

func testClientStreamMethod(t testing.TB) protoreflect.MethodDescriptor {
	method := testMethod(t, false, false, false)
	file := protodesc.ToFileDescriptorProto(method.ParentFile())
	file.Service[0].Method[0].ClientStreaming = proto.Bool(true)
	descriptor, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.Services().Get(0).Methods().Get(0)
}

func testMethodWithoutFieldMask(t testing.TB) protoreflect.MethodDescriptor {
	t.Helper()
	method := testMethod(t, false, false, false)
	file := protodesc.ToFileDescriptorProto(method.ParentFile())
	file.MessageType[0].Field = file.MessageType[0].Field[:3]
	descriptor, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.Services().Get(0).Methods().Get(0)
}

func testResponse(method protoreflect.MethodDescriptor, count, name string) proto.Message {
	message := dynamicpb.NewMessage(method.Output())
	_ = (protojson.UnmarshalOptions{}).Unmarshal([]byte(`{"count":"`+count+`","name":"`+name+`","count32":4294967295}`), message)
	return message
}

type fakeConnection struct {
	invoke           func(context.Context, any, any) error
	stream           *fakeClientStream
	newStreamErr     error
	responseMetadata metadata.MD
}

func (f *fakeConnection) Invoke(ctx context.Context, _ string, args, reply any, options ...grpc.CallOption) error {
	for _, option := range options {
		switch value := option.(type) {
		case grpc.HeaderCallOption:
			*value.HeaderAddr = f.responseMetadata
		case grpc.TrailerCallOption:
			*value.TrailerAddr = f.responseMetadata
		}
	}
	if f.invoke != nil {
		return f.invoke(ctx, args, reply)
	}
	return nil
}

func (f *fakeConnection) NewStream(ctx context.Context, _ *grpc.StreamDesc, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	if f.newStreamErr != nil {
		return nil, f.newStreamErr
	}
	f.stream.ctx = ctx
	return f.stream, nil
}

type fakeClientStream struct {
	ctx       context.Context
	responses []proto.Message
	index     int
	closed    bool
	mu        sync.Mutex
}

func (f *fakeClientStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeClientStream) Trailer() metadata.MD         { return nil }
func (f *fakeClientStream) Context() context.Context     { return f.ctx }
func (f *fakeClientStream) SendMsg(any) error            { return nil }
func (f *fakeClientStream) CloseSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *fakeClientStream) RecvMsg(output any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.index == len(f.responses) {
		return io.EOF
	}
	proto.Merge(output.(proto.Message), f.responses[f.index])
	f.index++
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type trailerReader struct {
	reader   *bytes.Reader
	trailers http.Header
}

func (r *trailerReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		r.trailers.Set("X-Protected-Metadata", "credential-secret")
	}
	return count, err
}

func protoResponse(statusCode int, mediaType string, payload []byte) *http.Response {
	return &http.Response{StatusCode: statusCode, Header: http.Header{"Content-Type": {mediaType}}, Body: io.NopCloser(bytes.NewReader(payload))}
}

func assertPublicError(t testing.TB, err error, code string, rpcCode RPCCode) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != code || failure.RPCCode != rpcCode || strings.Contains(err.Error(), "secret") || errors.Unwrap(err) != nil {
		t.Fatalf("public error = %#v / %v", failure, err)
	}
	raw, marshalErr := json.Marshal(failure)
	if marshalErr != nil || strings.Contains(string(raw), "secret") {
		t.Fatalf("marshaled error = %s / %v", raw, marshalErr)
	}
}

func errorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func hasDiagnostic(report Report, code string) bool {
	return slices.ContainsFunc(report.Diagnostics, func(value Diagnostic) bool { return value.Code == code })
}

func hasMapping(report Report, path, oneof, target, presence string) bool {
	return slices.ContainsFunc(report.Mappings, func(value FieldMapping) bool {
		return value.Path == path && value.Oneof == oneof && value.NaatreType == target && value.Presence == presence
	})
}

func mustReport(t testing.TB, report Report) []byte {
	t.Helper()
	value, err := MarshalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
