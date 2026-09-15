// Package protobufrpc maps protobuf descriptors onto the protocol-neutral
// interoperability adapter contract and supplies bounded gRPC and Connect
// clients. Descriptors remain the only schema authority: this package never
// infers Naatre operation policy from RPC names or protobuf options.
package protobufrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	Profile                 = "core.adapters.protobuf-grpc-connect-1"
	ProtobufRevision        = "google.golang.org/protobuf@v1.36.11"
	GRPCRevision            = "google.golang.org/grpc@v1.82.1"
	ConnectProtocolRevision = "connectrpc/connect@fac060371d74da4205f28ef504d078d2d2ce286f"
)

type Transport string

const (
	GRPC    Transport = "grpc"
	Connect Transport = "connect"
)

type Limits struct {
	MaxRequestBytes   int
	MaxResponseBytes  int
	MaxMetadataBytes  int
	MaxMetadataItems  int
	MaxStreamMessages int
	MaxStreamBytes    int64
}

type MetadataProvider func(context.Context) map[string][]string

type Config struct {
	Method          protoreflect.MethodDescriptor
	FieldMask       protoreflect.Name
	ForwardMetadata []string
	Metadata        MetadataProvider
	Limits          Limits
	HTTPClient      *http.Client
	ConnectURL      string
}

type Diagnostic struct {
	Code    string `json:"code"`
	Feature string `json:"feature,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type FieldMapping struct {
	Path       string `json:"path"`
	ProtoKind  string `json:"protoKind"`
	NaatreType string `json:"naatreType"`
	Presence   string `json:"presence"`
	Oneof      string `json:"oneof,omitempty"`
	Repeated   bool   `json:"repeated,omitempty"`
}

type Report struct {
	Profile      string            `json:"profile"`
	Transport    Transport         `json:"transport"`
	Status       string            `json:"status"`
	Method       string            `json:"method"`
	Streaming    string            `json:"streaming"`
	Dependencies map[string]string `json:"dependencies"`
	Mappings     []FieldMapping    `json:"mappings"`
	Diagnostics  []Diagnostic      `json:"diagnostics"`
}

type RPCCode string

const (
	CodeCanceled           RPCCode = "canceled"
	CodeUnknown            RPCCode = "unknown"
	CodeInvalidArgument    RPCCode = "invalid_argument"
	CodeDeadlineExceeded   RPCCode = "deadline_exceeded"
	CodeNotFound           RPCCode = "not_found"
	CodeAlreadyExists      RPCCode = "already_exists"
	CodePermissionDenied   RPCCode = "permission_denied"
	CodeResourceExhausted  RPCCode = "resource_exhausted"
	CodeFailedPrecondition RPCCode = "failed_precondition"
	CodeAborted            RPCCode = "aborted"
	CodeOutOfRange         RPCCode = "out_of_range"
	CodeUnimplemented      RPCCode = "unimplemented"
	CodeInternal           RPCCode = "internal"
	CodeUnavailable        RPCCode = "unavailable"
	CodeDataLoss           RPCCode = "data_loss"
	CodeUnauthenticated    RPCCode = "unauthenticated"
)

type Error struct {
	Code    string  `json:"code"`
	RPCCode RPCCode `json:"rpcCode,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "protobuf RPC adapter error"
	}
	return "protobuf RPC adapter: " + e.Code
}

type Adapter struct {
	transport       Transport
	method          protoreflect.MethodDescriptor
	fieldMask       protoreflect.FieldDescriptor
	forwardMetadata []string
	metadata        MetadataProvider
	limits          Limits
	grpc            grpc.ClientConnInterface
	http            *http.Client
	connectURL      string
	report          Report
}

type Stream struct {
	recv      func() (proto.Message, error)
	close     func() error
	limits    Limits
	messages  int
	bytes     int64
	closeOnce sync.Once
	closeErr  error
}

func NewGRPC(config Config, connection grpc.ClientConnInterface) (*Adapter, Report, error) {
	return newAdapter(config, GRPC, connection)
}

func NewConnect(config Config) (*Adapter, Report, error) {
	return newAdapter(config, Connect, nil)
}

func newAdapter(config Config, transport Transport, connection grpc.ClientConnInterface) (*Adapter, Report, error) {
	report := inspect(config, transport)
	if transport == GRPC && connection == nil {
		report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_TRANSPORT_REQUIRED", "transport", "", "a gRPC client connection is required"))
	}
	if transport == Connect {
		if err := validateConnectURL(config.ConnectURL, config.Method); err != nil {
			report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_ENDPOINT_INVALID", "transport", "", "Connect requires one absolute HTTPS endpoint without credentials, query, or fragment"))
		}
	}
	sortDiagnostics(report.Diagnostics)
	if len(report.Diagnostics) != 0 {
		report.Status = "rejected"
		return nil, report, publicError(report.Diagnostics[0].Code, "")
	}
	report.Status = "ready"
	mask := fieldMask(config.Method, config.FieldMask)
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	copyClient := *httpClient
	copyClient.Jar = nil
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect rejected") }
	adapter := &Adapter{
		transport: transport, method: config.Method, fieldMask: mask,
		forwardMetadata: normalizeMetadataNames(config.ForwardMetadata), metadata: config.Metadata,
		limits: resolveLimits(config.Limits), grpc: connection, http: &copyClient,
		connectURL: config.ConnectURL, report: report,
	}
	return adapter, adapter.Report(), nil
}

func (a *Adapter) Report() Report {
	if a == nil {
		return Report{}
	}
	result := a.report
	result.Dependencies = map[string]string{}
	for key, value := range a.report.Dependencies {
		result.Dependencies[key] = value
	}
	result.Mappings = slices.Clone(a.report.Mappings)
	result.Diagnostics = slices.Clone(a.report.Diagnostics)
	return result
}

func MarshalReport(report Report) ([]byte, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return nil, publicError("PROTO_RPC_REPORT_INVALID", "")
	}
	canonical, err := protocol.CanonicalizeJSON(raw, protocol.Limits{MaxBytes: 1 << 20})
	if err != nil {
		return nil, publicError("PROTO_RPC_REPORT_INVALID", "")
	}
	return canonical, nil
}

func (a *Adapter) Invoke(ctx context.Context, request interopadapter.BackendRequest) (map[string]any, error) {
	if a == nil || ctx == nil {
		return nil, publicError("PROTO_RPC_REQUEST_INVALID", "")
	}
	if a.method.IsStreamingClient() || a.method.IsStreamingServer() {
		return nil, publicError("PROTO_RPC_STREAMING_REQUIRED", "")
	}
	input, err := a.requestMessage(request)
	if err != nil {
		return nil, err
	}
	var output proto.Message
	switch a.transport {
	case GRPC:
		output, err = a.invokeGRPC(ctx, input)
	case Connect:
		output, err = a.invokeConnect(ctx, input)
	default:
		err = publicError("PROTO_RPC_TRANSPORT_UNSUPPORTED", "")
	}
	if err != nil {
		return nil, err
	}
	return a.responseMap(output)
}

func (a *Adapter) OpenStream(ctx context.Context, request interopadapter.BackendRequest) (*Stream, error) {
	if a == nil || ctx == nil {
		return nil, publicError("PROTO_RPC_REQUEST_INVALID", "")
	}
	if a.method.IsStreamingClient() || !a.method.IsStreamingServer() {
		return nil, publicError("PROTO_RPC_STREAMING_UNSUPPORTED", "")
	}
	input, err := a.requestMessage(request)
	if err != nil {
		return nil, err
	}
	switch a.transport {
	case GRPC:
		return a.openGRPCStream(ctx, input)
	case Connect:
		return a.openConnectStream(ctx, input)
	default:
		return nil, publicError("PROTO_RPC_TRANSPORT_UNSUPPORTED", "")
	}
}

func (s *Stream) Recv() (map[string]any, error) {
	if s == nil || s.recv == nil {
		return nil, publicError("PROTO_RPC_STREAM_INVALID", "")
	}
	if s.messages >= s.limits.MaxStreamMessages {
		_ = s.Close()
		return nil, publicError("PROTO_RPC_STREAM_LIMIT", string(CodeResourceExhausted))
	}
	message, err := s.recv()
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	size := int64(proto.Size(message))
	if size > int64(s.limits.MaxResponseBytes) || size > s.limits.MaxStreamBytes-s.bytes {
		_ = s.Close()
		return nil, publicError("PROTO_RPC_STREAM_LIMIT", string(CodeResourceExhausted))
	}
	s.messages++
	s.bytes += size
	return messageMap(message, s.limits.MaxResponseBytes)
}

func (s *Stream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.close != nil {
			if err := s.close(); err != nil {
				s.closeErr = publicError("PROTO_RPC_STREAM_CLOSE", string(CodeUnknown))
			}
		}
	})
	return s.closeErr
}

func inspect(config Config, transport Transport) Report {
	report := Report{
		Profile: Profile, Transport: transport, Status: "rejected",
		Dependencies: map[string]string{
			"protobuf": ProtobufRevision, "grpc": GRPCRevision, "connectProtocol": ConnectProtocolRevision,
		},
		Mappings: []FieldMapping{}, Diagnostics: []Diagnostic{},
	}
	if config.Method == nil {
		report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_METHOD_REQUIRED", "schema", "", "one protobuf method descriptor is required"))
		return report
	}
	report.Method = "/" + string(config.Method.Parent().FullName()) + "/" + string(config.Method.Name())
	switch {
	case config.Method.IsStreamingClient() && config.Method.IsStreamingServer():
		report.Streaming = "bidirectional"
		report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_STREAMING_UNSUPPORTED", "streaming", "", "bidirectional streaming is outside this profile"))
	case config.Method.IsStreamingClient():
		report.Streaming = "client"
		report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_STREAMING_UNSUPPORTED", "streaming", "", "client streaming is outside this profile"))
	case config.Method.IsStreamingServer():
		report.Streaming = "server"
	default:
		report.Streaming = "unary"
	}
	limits := resolveLimits(config.Limits)
	if config.Limits != (Limits{}) && limits != config.Limits {
		report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_LIMIT_INVALID", "resource-limits", "", "all configured limits must be positive and within profile ceilings"))
	}
	names, nameDiagnostics := validateMetadataNames(config.ForwardMetadata)
	_ = names
	report.Diagnostics = append(report.Diagnostics, nameDiagnostics...)
	mask := fieldMask(config.Method, config.FieldMask)
	if config.FieldMask != "" && mask == nil {
		report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_FIELD_MASK_INVALID", "field-masks", string(config.FieldMask), "field-mask target must be a singular google.protobuf.FieldMask request field"))
	}
	seen := map[protoreflect.FullName]bool{}
	inspectMessage(config.Method.Input(), "request", true, config.FieldMask, seen, &report)
	clear(seen)
	inspectMessage(config.Method.Output(), "response", false, "", seen, &report)
	sort.Slice(report.Mappings, func(i, j int) bool { return report.Mappings[i].Path < report.Mappings[j].Path })
	return report
}

func inspectMessage(message protoreflect.MessageDescriptor, prefix string, input bool, maskName protoreflect.Name, seen map[protoreflect.FullName]bool, report *Report) {
	if seen[message.FullName()] {
		return
	}
	seen[message.FullName()] = true
	fields := message.Fields()
	for index := 0; index < fields.Len(); index++ {
		field := fields.Get(index)
		path := prefix + "." + string(field.Name())
		mapping := FieldMapping{Path: path, ProtoKind: field.Kind().String(), Repeated: field.Cardinality() == protoreflect.Repeated}
		if field.HasPresence() {
			mapping.Presence = "explicit"
		} else {
			mapping.Presence = "implicit-default"
		}
		if oneof := field.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() {
			mapping.Oneof = string(oneof.Name())
		}
		mapping.NaatreType = naatreType(field)
		report.Mappings = append(report.Mappings, mapping)
		if field.IsMap() {
			report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_MAP_UNSUPPORTED", "shape", path, "protobuf map fields are outside the lossless profile"))
			continue
		}
		if field.Kind() == protoreflect.BytesKind {
			report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_BYTES_UNSUPPORTED", "shape", path, "protobuf padded base64 and Naatre base64url are not silently adapted"))
		}
		if input && !field.HasPresence() && field.Cardinality() != protoreflect.Repeated {
			report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_PRESENCE_UNSUPPORTED", "presence", path, "implicit-presence request fields cannot preserve absent versus present default"))
		}
		if field.Kind() != protoreflect.MessageKind && field.Kind() != protoreflect.GroupKind {
			continue
		}
		fullName := field.Message().FullName()
		if field.Name() == maskName && fullName == "google.protobuf.FieldMask" || fullName == "google.protobuf.Timestamp" || fullName == "google.protobuf.Duration" {
			continue
		}
		if strings.HasPrefix(string(fullName), "google.protobuf.") {
			report.Diagnostics = append(report.Diagnostics, diagnostic("PROTO_RPC_WELL_KNOWN_TYPE_UNSUPPORTED", "shape", path, "this protobuf well-known type is outside the lossless profile"))
			continue
		}
		inspectMessage(field.Message(), path, input, "", seen, report)
	}
}

func naatreType(field protoreflect.FieldDescriptor) string {
	base := ""
	switch field.Kind() {
	case protoreflect.BoolKind:
		base = "Boolean"
	case protoreflect.EnumKind:
		base = string(field.Enum().FullName())
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		base = "Int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		base = "UInt64"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		base = "Int64"
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		base = "Float64"
	case protoreflect.StringKind:
		base = "String"
	case protoreflect.BytesKind:
		base = "unsupported"
	case protoreflect.MessageKind, protoreflect.GroupKind:
		switch field.Message().FullName() {
		case "google.protobuf.Timestamp":
			base = "Timestamp"
		case "google.protobuf.Duration":
			base = "Duration"
		case "google.protobuf.FieldMask":
			base = "FieldMask"
		default:
			base = string(field.Message().FullName())
		}
	default:
		base = "unsupported"
	}
	if field.Cardinality() == protoreflect.Repeated {
		return "List<" + base + ">"
	}
	return base
}

func (a *Adapter) requestMessage(request interopadapter.BackendRequest) (proto.Message, error) {
	message := dynamicpb.NewMessage(a.method.Input())
	if len(request.Input) > a.limits.MaxRequestBytes {
		return nil, publicError("PROTO_RPC_REQUEST_LIMIT", string(CodeResourceExhausted))
	}
	if len(request.Input) != 0 {
		options := protojson.UnmarshalOptions{DiscardUnknown: false}
		if err := options.Unmarshal(request.Input, message); err != nil {
			return nil, publicError("PROTO_RPC_SHAPE_INVALID", string(CodeInvalidArgument))
		}
	}
	if len(message.GetUnknown()) != 0 || !finiteMessage(message.ProtoReflect()) {
		return nil, publicError("PROTO_RPC_SHAPE_INVALID", string(CodeInvalidArgument))
	}
	if len(request.Projection) != 0 && a.fieldMask == nil {
		return nil, publicError("PROTO_RPC_FIELD_MASK_REQUIRED", string(CodeInvalidArgument))
	}
	if a.fieldMask != nil && message.Has(a.fieldMask) {
		return nil, publicError("PROTO_RPC_FIELD_MASK_CONFLICT", string(CodeInvalidArgument))
	}
	if a.fieldMask != nil && len(request.Projection) != 0 {
		if !sort.StringsAreSorted(request.Projection) || len(slices.Compact(slices.Clone(request.Projection))) != len(request.Projection) {
			return nil, publicError("PROTO_RPC_FIELD_MASK_INVALID", string(CodeInvalidArgument))
		}
		for _, path := range request.Projection {
			if !validProjectionPath(a.method.Output(), path) {
				return nil, publicError("PROTO_RPC_FIELD_MASK_INVALID", string(CodeInvalidArgument))
			}
		}
		maskMessage := message.Mutable(a.fieldMask).Message()
		list := maskMessage.Mutable(maskMessage.Descriptor().Fields().ByName("paths")).List()
		list.Truncate(0)
		for _, path := range request.Projection {
			list.Append(protoreflect.ValueOfString(path))
		}
	}
	if proto.Size(message) > a.limits.MaxRequestBytes {
		return nil, publicError("PROTO_RPC_REQUEST_LIMIT", string(CodeResourceExhausted))
	}
	return message, nil
}

func (a *Adapter) responseMap(message proto.Message) (map[string]any, error) {
	return messageMap(message, a.limits.MaxResponseBytes)
}

func messageMap(message proto.Message, maximum int) (map[string]any, error) {
	if message == nil || len(message.ProtoReflect().GetUnknown()) != 0 || !finiteMessage(message.ProtoReflect()) {
		return nil, publicError("PROTO_RPC_RESPONSE_INVALID", string(CodeDataLoss))
	}
	raw, err := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}).Marshal(message)
	if err != nil || len(raw) > maximum {
		if len(raw) > maximum {
			return nil, publicError("PROTO_RPC_RESPONSE_LIMIT", string(CodeResourceExhausted))
		}
		return nil, publicError("PROTO_RPC_RESPONSE_INVALID", string(CodeDataLoss))
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, publicError("PROTO_RPC_RESPONSE_INVALID", string(CodeDataLoss))
	}
	if !normalizeUnsigned32(message.ProtoReflect().Descriptor(), result) {
		return nil, publicError("PROTO_RPC_RESPONSE_INVALID", string(CodeDataLoss))
	}
	return result, nil
}

func normalizeUnsigned32(descriptor protoreflect.MessageDescriptor, value map[string]any) bool {
	fields := descriptor.Fields()
	for index := 0; index < fields.Len(); index++ {
		field := fields.Get(index)
		key := string(field.Name())
		member, present := value[key]
		if !present {
			continue
		}
		values := []any{member}
		if field.IsList() {
			list, ok := member.([]any)
			if !ok {
				return false
			}
			values = list
		}
		for itemIndex, item := range values {
			switch field.Kind() {
			case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
				number, ok := item.(json.Number)
				if !ok {
					return false
				}
				parsed, err := strconv.ParseUint(string(number), 10, 32)
				if err != nil {
					return false
				}
				values[itemIndex] = strconv.FormatUint(parsed, 10)
			case protoreflect.MessageKind, protoreflect.GroupKind:
				if fullName := field.Message().FullName(); fullName == "google.protobuf.Timestamp" || fullName == "google.protobuf.Duration" || fullName == "google.protobuf.FieldMask" {
					continue
				}
				object, ok := item.(map[string]any)
				if !ok || !normalizeUnsigned32(field.Message(), object) {
					return false
				}
			case protoreflect.BoolKind, protoreflect.EnumKind, protoreflect.Int32Kind,
				protoreflect.Sint32Kind, protoreflect.Int64Kind, protoreflect.Sint64Kind,
				protoreflect.Uint64Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind,
				protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind, protoreflect.DoubleKind,
				protoreflect.StringKind, protoreflect.BytesKind:
				// These protobuf JSON shapes already match their declared Naatre mapping.
			}
		}
		if field.IsList() {
			value[key] = values
		} else {
			value[key] = values[0]
		}
	}
	return true
}

func finiteMessage(message protoreflect.Message) bool {
	valid := true
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() {
			valid = false
			return false
		}
		check := func(value protoreflect.Value) bool {
			switch field.Kind() {
			case protoreflect.FloatKind, protoreflect.DoubleKind:
				return !math.IsInf(value.Float(), 0) && !math.IsNaN(value.Float())
			case protoreflect.MessageKind, protoreflect.GroupKind:
				return finiteMessage(value.Message())
			case protoreflect.BoolKind, protoreflect.EnumKind, protoreflect.Int32Kind,
				protoreflect.Sint32Kind, protoreflect.Uint32Kind, protoreflect.Int64Kind,
				protoreflect.Sint64Kind, protoreflect.Uint64Kind, protoreflect.Sfixed32Kind,
				protoreflect.Fixed32Kind, protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind,
				protoreflect.StringKind, protoreflect.BytesKind:
				return true
			}
			return false
		}
		if field.IsList() {
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				if !check(list.Get(index)) {
					valid = false
					return false
				}
			}
			return true
		}
		valid = check(value)
		return valid
	})
	return valid
}

func fieldMask(method protoreflect.MethodDescriptor, name protoreflect.Name) protoreflect.FieldDescriptor {
	if method == nil || name == "" {
		return nil
	}
	field := method.Input().Fields().ByName(name)
	if field == nil || field.Cardinality() == protoreflect.Repeated || field.Kind() != protoreflect.MessageKind || field.Message().FullName() != "google.protobuf.FieldMask" {
		return nil
	}
	return field
}

func resolveLimits(input Limits) Limits {
	defaults := Limits{MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxMetadataBytes: 16 << 10, MaxMetadataItems: 64, MaxStreamMessages: 1024, MaxStreamBytes: 16 << 20}
	if input == (Limits{}) {
		return defaults
	}
	if input.MaxRequestBytes < 1 || input.MaxRequestBytes > 16<<20 || input.MaxResponseBytes < 1 || input.MaxResponseBytes > 16<<20 || input.MaxMetadataBytes < 1 || input.MaxMetadataBytes > 1<<20 || input.MaxMetadataItems < 1 || input.MaxMetadataItems > 1024 || input.MaxStreamMessages < 1 || input.MaxStreamMessages > 1_000_000 || input.MaxStreamBytes < 1 || input.MaxStreamBytes > 1<<30 {
		return defaults
	}
	return input
}

var metadataNamePattern = regexp.MustCompile(`^[0-9a-z][0-9a-z_.-]{0,126}[0-9a-z]$|^[0-9a-z]$`)

func validateMetadataNames(input []string) ([]string, []Diagnostic) {
	forbidden := map[string]bool{"cookie": true, "connection": true, "content-length": true, "grpc-timeout": true, "host": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true}
	seen := map[string]bool{}
	result := make([]string, 0, len(input))
	diagnostics := []Diagnostic{}
	for _, raw := range input {
		name := strings.ToLower(raw)
		if raw != name || !metadataNamePattern.MatchString(name) || strings.HasPrefix(name, "grpc-") || strings.HasPrefix(name, ":") || strings.HasSuffix(name, "-bin") || forbidden[name] || seen[name] {
			diagnostics = append(diagnostics, diagnostic("PROTO_RPC_METADATA_POLICY_INVALID", "metadata", raw, "metadata allowlist entry is invalid, reserved, binary, unsafe, or duplicated"))
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	sort.Strings(result)
	return result, diagnostics
}

func normalizeMetadataNames(input []string) []string {
	result, _ := validateMetadataNames(input)
	return result
}

func (a *Adapter) outgoingMetadata(ctx context.Context) (context.Context, http.Header, error) {
	values := map[string][]string{}
	if a.metadata != nil {
		values = a.metadata(ctx)
	}
	items := 0
	bytes := 0
	pairs := []string{}
	headers := http.Header{}
	for _, name := range a.forwardMetadata {
		for _, value := range values[name] {
			if strings.ContainsAny(value, "\r\n\x00") {
				return nil, nil, publicError("PROTO_RPC_METADATA_INVALID", string(CodeInvalidArgument))
			}
			items++
			bytes += len(name) + len(value)
			if items > a.limits.MaxMetadataItems || bytes > a.limits.MaxMetadataBytes {
				return nil, nil, publicError("PROTO_RPC_METADATA_LIMIT", string(CodeResourceExhausted))
			}
			pairs = append(pairs, name, value)
			headers.Add(name, value)
		}
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(pairs...)), headers, nil
}

func classifyGRPCError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return publicError("PROTO_RPC_CANCELLED", string(CodeCanceled))
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return publicError("PROTO_RPC_DEADLINE_EXCEEDED", string(CodeDeadlineExceeded))
	}
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return publicError("PROTO_RPC_TRANSPORT_FAILURE", string(CodeUnavailable))
	}
	if len(grpcStatus.Proto().Details) != 0 {
		return publicError("PROTO_RPC_STATUS_DETAILS_UNSUPPORTED", string(rpcCode(grpcStatus.Code())))
	}
	return publicError("PROTO_RPC_STATUS", string(rpcCode(grpcStatus.Code())))
}

func rpcCode(code codes.Code) RPCCode {
	values := [...]RPCCode{CodeUnknown, CodeCanceled, CodeUnknown, CodeInvalidArgument, CodeDeadlineExceeded, CodeNotFound, CodeAlreadyExists, CodePermissionDenied, CodeResourceExhausted, CodeFailedPrecondition, CodeAborted, CodeOutOfRange, CodeUnimplemented, CodeInternal, CodeUnavailable, CodeDataLoss, CodeUnauthenticated}
	if int(code) < 0 || int(code) >= len(values) {
		return CodeUnknown
	}
	return values[code]
}

func publicError(code, rpc string) *Error { return &Error{Code: code, RPCCode: RPCCode(rpc)} }

func diagnostic(code, feature, field, message string) Diagnostic {
	return Diagnostic{Code: code, Feature: feature, Field: field, Message: message}
}

func sortDiagnostics(values []Diagnostic) {
	sort.Slice(values, func(i, j int) bool {
		return values[i].Code+"\x00"+values[i].Field < values[j].Code+"\x00"+values[j].Field
	})
}

func validProjectionPath(descriptor protoreflect.MessageDescriptor, value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	segments := strings.Split(value, ".")
	for index, segment := range segments {
		if !protoreflect.Name(segment).IsValid() {
			return false
		}
		field := descriptor.Fields().ByName(protoreflect.Name(segment))
		if field == nil || field.IsMap() {
			return false
		}
		if index == len(segments)-1 {
			continue
		}
		if field.Kind() != protoreflect.MessageKind && field.Kind() != protoreflect.GroupKind {
			return false
		}
		descriptor = field.Message()
	}
	return true
}

func fullMethod(method protoreflect.MethodDescriptor) string {
	return "/" + string(method.Parent().FullName()) + "/" + string(method.Name())
}

func grpcStreamError(ctx context.Context, err error) error {
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return classifyGRPCError(ctx, err)
}

func deadlineHeader(ctx context.Context) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ""
	}
	millis := max(time.Until(deadline).Milliseconds(), 1)
	if millis > 9_999_999_999 {
		millis = 9_999_999_999
	}
	return fmt.Sprintf("%d", millis)
}
