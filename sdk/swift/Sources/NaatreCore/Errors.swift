import Foundation

public struct ClientFailure: Error, Codable, Equatable, Sendable {
    public enum Code: String, Codable, Sendable {
        case cancelled = "CLIENT_CANCELLED"
        case deadlineExceeded = "CLIENT_DEADLINE_EXCEEDED"
        case responseTooLarge = "CLIENT_RESPONSE_TOO_LARGE"
        case frameTooLarge = "CLIENT_FRAME_TOO_LARGE"
        case streamTruncated = "CLIENT_STREAM_TRUNCATED"
        case protocolInvalid = "CLIENT_PROTOCOL_INVALID"
        case resultInvalid = "CLIENT_RESULT_INVALID"
        case scalarInvalid = "CLIENT_SCALAR_INVALID"
        case valuePrecision = "CLIENT_VALUE_PRECISION"
        case unsupportedCapability = "CLIENT_UNSUPPORTED_CAPABILITY"
        case transport = "CLIENT_TRANSPORT"
    }

    public let code: Code

    public init(_ code: Code) {
        self.code = code
    }
}

public enum UnsupportedCapability: String, Codable, Sendable {
    case urlSession = "urlsession-transport"
    case serverSentEvents = "sse-transport"
    case webSocket = "websocket-transport"
    case automaticAuthenticationRefresh = "automatic-auth-refresh"
    case automaticWriteReplay = "automatic-write-replay"
    case appleLifecycleAdapter = "apple-lifecycle-adapter"
}
