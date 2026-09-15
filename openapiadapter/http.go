package openapiadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/schema"
)

var (
	errRedirect = errors.New("openapi redirect denied")
	errFanOut   = errors.New("openapi projection fan-out exceeded")
)

const maxHTTPBodyBytes int64 = 1 << 30

type httpInvoker struct {
	endpoint            *url.URL
	client              *http.Client
	operation           httpOperation
	credentialHeaders   []string
	credentials         CredentialProvider
	maxRequestBytes     int64
	maxResponseBytes    int64
	maxProjectionFields int
}

func newHTTPInvoker(config HTTPConfig, operation httpOperation) (*httpInvoker, error) {
	endpoint, err := url.Parse(config.BaseURL)
	if err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, publicError("OPENAPI_ENDPOINT_INVALID", errors.New("base URL is invalid"))
	}
	origin := endpoint.Scheme + "://" + endpoint.Host
	allowed := false
	for _, candidate := range config.AllowedOrigins {
		parsed, parseErr := url.Parse(candidate)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return nil, publicError("OPENAPI_EGRESS_POLICY_INVALID", errors.New("allowed origins must be exact origins"))
		}
		if parsed.Scheme+"://"+parsed.Host == origin {
			allowed = true
		}
	}
	if !allowed {
		return nil, publicError("OPENAPI_EGRESS_DENIED", errors.New("base URL origin is not allowlisted"))
	}
	if config.MaxRequestBytes < 1 || config.MaxRequestBytes > maxHTTPBodyBytes || config.MaxResponseBytes < 1 || config.MaxResponseBytes > maxHTTPBodyBytes {
		return nil, publicError("OPENAPI_LIMIT_INVALID", errors.New("HTTP byte limits must be positive"))
	}
	if operation.successStatus < 200 || operation.successStatus > 299 {
		return nil, publicError("OPENAPI_RESPONSE_UNSUPPORTED", errors.New("one exact success status is required"))
	}
	maximumProjection := config.MaxProjectionFields
	if maximumProjection == 0 {
		maximumProjection = 4096
	}
	if maximumProjection < 1 || maximumProjection > 4096 {
		return nil, publicError("OPENAPI_LIMIT_INVALID", errors.New("projection limit is invalid"))
	}
	headers, err := normalizeCredentialHeaders(config.CredentialHeaders)
	if err != nil {
		return nil, err
	}
	if operation.securityRequired && (config.Credentials == nil || len(headers) == 0) {
		return nil, publicError("OPENAPI_CREDENTIALS_REQUIRED", errors.New("secured operation requires configured credential headers"))
	}
	if operation.securityRequired && len(operation.securityHeaders) != 0 && !matchesSecurityHeaders(headers, operation.securityHeaders) {
		return nil, publicError("OPENAPI_CREDENTIALS_REQUIRED", errors.New("credential headers do not satisfy a declared security requirement"))
	}
	client := http.DefaultClient
	if config.Client != nil {
		client = config.Client
	}
	copyClient := *client
	copyClient.Jar = nil
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	resolved := *endpoint
	resolved.Path = strings.TrimRight(endpoint.Path, "/") + operation.path
	return &httpInvoker{
		endpoint: &resolved, client: &copyClient, operation: operation,
		credentialHeaders: headers, credentials: config.Credentials,
		maxRequestBytes: config.MaxRequestBytes, maxResponseBytes: config.MaxResponseBytes,
		maxProjectionFields: maximumProjection,
	}, nil
}

func (i *httpInvoker) Invoke(ctx context.Context, request interopadapter.BackendRequest) (map[string]any, error) {
	if i == nil || ctx == nil || request.Operation != i.operation.id {
		return nil, publicError("OPENAPI_REQUEST_INVALID", errors.New("invoker, context, and operation must match"))
	}
	projection, err := normalizeProjection(request.Projection, i.maxProjectionFields)
	if err != nil {
		return nil, publicError("OPENAPI_FANOUT_LIMIT", err)
	}
	if len(projection) != 0 && i.operation.fieldMaskQuery == "" {
		return nil, publicError("OPENAPI_PROJECTION_UNSUPPORTED", errors.New("operation has no declared field-mask parameter"))
	}
	var body io.Reader
	if len(i.operation.requestSchema) == 0 {
		if len(request.Input) != 0 {
			return nil, publicError("OPENAPI_REQUEST_INVALID", errors.New("operation has no request body"))
		}
	} else {
		if len(request.Input) == 0 {
			return nil, publicError("OPENAPI_REQUEST_INVALID", errors.New("required request body is absent"))
		}
		if int64(len(request.Input)) > i.maxRequestBytes {
			return nil, publicError("OPENAPI_REQUEST_LIMIT", errors.New("request exceeds configured limit"))
		}
		transformed, transformErr := transformJSON(request.Input, i.operation.requestSchema, i.operation.components, outboundTransform, 0)
		if transformErr != nil {
			return nil, transformErr
		}
		if int64(len(transformed)) > i.maxRequestBytes {
			return nil, publicError("OPENAPI_REQUEST_LIMIT", errors.New("request exceeds configured limit"))
		}
		body = bytes.NewReader(transformed)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, i.operation.method, i.endpoint.String(), body)
	if err != nil {
		return nil, publicError("OPENAPI_REQUEST_INVALID", err)
	}
	if body != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	if len(projection) != 0 {
		query := httpRequest.URL.Query()
		query.Set(i.operation.fieldMaskQuery, strings.Join(projection, ","))
		httpRequest.URL.RawQuery = query.Encode()
	}
	if err := i.addCredentials(ctx, httpRequest); err != nil {
		return nil, err
	}
	response, err := i.client.Do(httpRequest)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		switch {
		case errors.Is(err, errRedirect):
			return nil, publicError("OPENAPI_REDIRECT_BLOCKED", err)
		case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
			return nil, publicError("OPENAPI_CANCELLED", err)
		case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
			return nil, publicError("OPENAPI_DEADLINE_EXCEEDED", err)
		default:
			return nil, publicError("OPENAPI_TRANSPORT_FAILED", err)
		}
	}
	if response == nil || response.Body == nil {
		return nil, publicError("OPENAPI_TRANSPORT_FAILED", errors.New("transport returned an incomplete response"))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != i.operation.successStatus {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, i.maxResponseBytes+1))
		return nil, publicError("OPENAPI_UPSTREAM_STATUS", errors.New("upstream returned a non-success status"))
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, publicError("OPENAPI_RESPONSE_MEDIA_TYPE", errors.New("success response is not application/json"))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, i.maxResponseBytes+1))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, publicError("OPENAPI_CANCELLED", err)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, publicError("OPENAPI_DEADLINE_EXCEEDED", err)
		}
		return nil, publicError("OPENAPI_RESPONSE_READ_FAILED", err)
	}
	if int64(len(payload)) > i.maxResponseBytes {
		return nil, publicError("OPENAPI_RESPONSE_LIMIT", errors.New("response exceeds configured limit"))
	}
	transformed, err := transformJSON(payload, i.operation.responseSchema, i.operation.components, inboundTransform, 0)
	if err != nil {
		return nil, err
	}
	var output map[string]any
	decoder := json.NewDecoder(bytes.NewReader(transformed))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil || output == nil {
		return nil, publicError("OPENAPI_RESPONSE_INVALID", errors.New("response must be a JSON object"))
	}
	return output, nil
}

func (i *httpInvoker) addCredentials(ctx context.Context, request *http.Request) error {
	if i.credentials == nil {
		return nil
	}
	provided := i.credentials(ctx)
	for _, name := range i.credentialHeaders {
		for _, value := range provided.Values(name) {
			if strings.ContainsAny(value, "\r\n") {
				return publicError("OPENAPI_CREDENTIAL_INVALID", errors.New("credential header contains a line break"))
			}
			request.Header.Add(name, value)
		}
	}
	if i.operation.securityRequired && !requestHasSecurityHeaders(request.Header, i.operation.securityHeaders, i.credentialHeaders) {
		return publicError("OPENAPI_CREDENTIALS_REQUIRED", errors.New("credential provider did not supply a declared security requirement"))
	}
	return nil
}

func matchesSecurityHeaders(configured []string, alternatives [][]string) bool {
	configuredSet := make(map[string]bool, len(configured))
	for _, name := range configured {
		configuredSet[http.CanonicalHeaderKey(name)] = true
	}
	for _, alternative := range alternatives {
		matched := len(alternative) != 0
		for _, name := range alternative {
			matched = matched && configuredSet[http.CanonicalHeaderKey(name)]
		}
		if matched {
			return true
		}
	}
	return false
}

func requestHasSecurityHeaders(headers http.Header, alternatives [][]string, configured []string) bool {
	if len(alternatives) == 0 {
		for _, name := range configured {
			if hasCredentialValue(headers.Values(name)) {
				return true
			}
		}
		return false
	}
	for _, alternative := range alternatives {
		matched := len(alternative) != 0
		for _, name := range alternative {
			matched = matched && hasCredentialValue(headers.Values(name))
		}
		if matched {
			return true
		}
	}
	return false
}

func hasCredentialValue(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func normalizeCredentialHeaders(input []string) ([]string, error) {
	result := make([]string, 0, len(input))
	seen := make(map[string]bool, len(input))
	for _, name := range input {
		canonical := http.CanonicalHeaderKey(name)
		if !validCredentialHeaderName(name) || seen[canonical] {
			return nil, publicError("OPENAPI_CREDENTIAL_HEADER_INVALID", errors.New("credential header is invalid or protected"))
		}
		seen[canonical] = true
		result = append(result, canonical)
	}
	sort.Strings(result)
	return result, nil
}

func validCredentialHeaderName(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	blocked := map[string]bool{
		"Accept": true, "Connection": true, "Content-Encoding": true, "Content-Length": true,
		"Content-Type": true, "Cookie": true, "Expect": true, "Forwarded": true, "Host": true,
		"Proxy-Authorization": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
		"Transfer-Encoding": true, "Upgrade": true, "Via": true, "X-Real-Ip": true,
	}
	return validHeaderName(name) && !blocked[canonical] && !strings.HasPrefix(canonical, "X-Forwarded-")
}

func normalizeProjection(input []string, maximum int) ([]string, error) {
	if maximum < 1 || len(input) > maximum {
		return nil, errFanOut
	}
	result := slices.Clone(input)
	for _, field := range result {
		if field == "" || len(field) > 512 || strings.ContainsAny(field, "&=,?#") {
			return nil, errors.New("projection field is invalid")
		}
	}
	sort.Strings(result)
	return slices.Compact(result), nil
}

type transformDirection uint8

const (
	outboundTransform transformDirection = iota + 1
	inboundTransform
)

func transformJSON(raw, rawSchema []byte, components map[string]jsonRaw, direction transformDirection, depth int) ([]byte, error) {
	if depth > 64 {
		return nil, publicError("OPENAPI_RESPONSE_INVALID", errors.New("value exceeds schema depth"))
	}
	value, err := decodeObject(rawSchema)
	if err != nil {
		return nil, publicError("OPENAPI_SCHEMA_INVALID", err)
	}
	if rawReference := value["$ref"]; len(rawReference) != 0 {
		var reference string
		if json.Unmarshal(rawReference, &reference) != nil || !strings.HasPrefix(reference, "#/components/schemas/") {
			return nil, publicError("OPENAPI_REFERENCE_BLOCKED", errors.New("runtime schema reference is blocked"))
		}
		component, ok := components[strings.TrimPrefix(reference, "#/components/schemas/")]
		if !ok {
			return nil, publicError("OPENAPI_REFERENCE_BLOCKED", errors.New("runtime schema reference is unresolved"))
		}
		return transformJSON(raw, component, components, direction, depth+1)
	}
	typeName, nullable, err := decodeSchemaType(value["type"])
	if err != nil {
		return nil, err
	}
	if bytes.Equal(raw, []byte("null")) {
		if !nullable {
			return nil, publicError(transformInvalidCode(direction), errors.New("non-null schema received null"))
		}
		return []byte("null"), nil
	}
	if len(value["enum"]) != 0 {
		var allowed []string
		var actual string
		if json.Unmarshal(value["enum"], &allowed) != nil || json.Unmarshal(raw, &actual) != nil || !slices.Contains(allowed, actual) {
			return nil, publicError(transformInvalidCode(direction), errors.New("enum value is invalid"))
		}
		return json.Marshal(actual)
	}
	switch typeName {
	case "object":
		return transformObject(raw, value, components, direction, depth)
	case "array":
		return transformArray(raw, value["items"], components, direction, depth)
	case "integer":
		var format string
		_ = json.Unmarshal(value["format"], &format)
		if format == "int64" || format == "uint64" {
			return transformExtendedInteger(raw, format, direction)
		}
	}
	identifier, err := scalarType(typeName, value["format"])
	if err != nil {
		return nil, err
	}
	parsed, err := schema.ParseScalar(schema.ScalarKind(identifier), raw)
	if err != nil || parsed.IsNull() {
		return nil, publicError(transformInvalidCode(direction), err)
	}
	return parsed.MarshalJSON()
}

func transformObject(raw []byte, schemaValue map[string]jsonRaw, components map[string]jsonRaw, direction transformDirection, depth int) ([]byte, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return nil, publicError(transformInvalidCode(direction), errors.New("object value is invalid"))
	}
	properties, err := decodeObject(schemaValue["properties"])
	if err != nil {
		return nil, publicError("OPENAPI_SCHEMA_INVALID", errors.New("object properties are invalid"))
	}
	for name := range object {
		if _, ok := properties[name]; !ok {
			return nil, publicError(transformInvalidCode(direction), errors.New("object contains an undeclared property"))
		}
	}
	var required []string
	_ = json.Unmarshal(schemaValue["required"], &required)
	for _, name := range required {
		if _, ok := object[name]; !ok {
			return nil, publicError(transformInvalidCode(direction), errors.New("required property is absent"))
		}
	}
	transformed := make(map[string]jsonRaw, len(object))
	for _, name := range sortedKeys(object) {
		mapped, transformErr := transformJSON(object[name], properties[name], components, direction, depth+1)
		if transformErr != nil {
			return nil, transformErr
		}
		transformed[name] = mapped
	}
	return marshalRawObject(transformed)
}

func transformArray(raw, itemSchema []byte, components map[string]jsonRaw, direction transformDirection, depth int) ([]byte, error) {
	var values []jsonRaw
	if json.Unmarshal(raw, &values) != nil {
		return nil, publicError(transformInvalidCode(direction), errors.New("array value is invalid"))
	}
	for index := range values {
		mapped, err := transformJSON(values[index], itemSchema, components, direction, depth+1)
		if err != nil {
			return nil, err
		}
		values[index] = mapped
	}
	return marshalRawArray(values), nil
}

func marshalRawObject(values map[string]jsonRaw) ([]byte, error) {
	var output bytes.Buffer
	output.WriteByte('{')
	for index, name := range sortedKeys(values) {
		if index != 0 {
			output.WriteByte(',')
		}
		encodedName, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		output.Write(encodedName)
		output.WriteByte(':')
		output.Write(values[name])
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func marshalRawArray(values []jsonRaw) []byte {
	var output bytes.Buffer
	output.WriteByte('[')
	for index, value := range values {
		if index != 0 {
			output.WriteByte(',')
		}
		output.Write(value)
	}
	output.WriteByte(']')
	return output.Bytes()
}

func transformExtendedInteger(raw []byte, format string, direction transformDirection) ([]byte, error) {
	kind := schema.Int64
	if format == "uint64" {
		kind = schema.UInt64
	}
	if direction == outboundTransform {
		value, err := schema.ParseScalar(kind, raw)
		if err != nil || value.IsNull() {
			return nil, publicError("OPENAPI_REQUEST_INVALID", errors.New("extended integer is invalid"))
		}
		encoded, _ := value.MarshalJSON()
		var digits string
		_ = json.Unmarshal(encoded, &digits)
		return []byte(digits), nil
	}
	if len(raw) == 0 || strings.ContainsAny(string(raw), ".eE+\"") {
		return nil, publicError("OPENAPI_RESPONSE_INVALID", errors.New("extended integer is invalid"))
	}
	quoted, _ := json.Marshal(string(raw))
	value, err := schema.ParseScalar(kind, quoted)
	if err != nil {
		return nil, publicError("OPENAPI_RESPONSE_INVALID", errors.New("extended integer is out of range"))
	}
	return value.MarshalJSON()
}

func transformInvalidCode(direction transformDirection) string {
	if direction == outboundTransform {
		return "OPENAPI_REQUEST_INVALID"
	}
	return "OPENAPI_RESPONSE_INVALID"
}
