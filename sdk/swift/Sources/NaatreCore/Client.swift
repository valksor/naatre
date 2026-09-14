import Foundation

public struct TransportLimits: Equatable, Sendable {
    public var responseBytes: Int
    public var frameBytes: Int
    public var redirects: Int

    public init(responseBytes: Int = 8 << 20, frameBytes: Int = 1 << 20, redirects: Int = 5) {
        self.responseBytes = responseBytes
        self.frameBytes = frameBytes
        self.redirects = redirects
    }
}

public struct TransportRequest: Sendable {
    public let id: UUID
    public let body: Data
    public let headers: [String: String]
    public let operationKind: OperationKind

    public init(id: UUID, body: Data, headers: [String: String], operationKind: OperationKind) {
        self.id = id
        self.body = body
        self.headers = headers
        self.operationKind = operationKind
    }
}

public protocol ClientTransport: Sendable {
    func execute(_ request: TransportRequest) async throws -> Data
    func stream(_ request: TransportRequest) async throws -> AsyncThrowingStream<Data, any Error>
    func cancel(requestID: UUID) async
    func cancelAll() async
}

public protocol AuthenticationProvider: Sendable {
    func headers() async throws -> [String: String]
}

public struct NoAuthentication: AuthenticationProvider {
    public init() {}
    public func headers() async throws -> [String: String] { [:] }
}

public enum BackgroundBehavior: Sendable {
    case cancelNetworkWork
    case continueIfAdapterPermits
}

public enum RedirectPolicy {
    public static func forwardedHeaders(
        _ headers: [String: String],
        from source: URL,
        to destination: URL,
        credentialOrigins: Set<String> = []
    ) -> [String: String] {
        guard source.origin != destination.origin, !credentialOrigins.contains(destination.origin) else { return headers }
        let sensitive = Set(["authorization", "cookie", "proxy-authorization", "x-api-key", "x-tenant-id"])
        return headers.filter { !sensitive.contains($0.key.lowercased()) }
    }
}

public actor NaatreClient {
    private let transport: any ClientTransport
    private let authentication: any AuthenticationProvider
    public let limits: TransportLimits
    public let backgroundBehavior: BackgroundBehavior

    public init(
        transport: any ClientTransport,
        authentication: any AuthenticationProvider = NoAuthentication(),
        limits: TransportLimits = TransportLimits(),
        backgroundBehavior: BackgroundBehavior = .cancelNetworkWork
    ) {
        self.transport = transport
        self.authentication = authentication
        self.limits = limits
        self.backgroundBehavior = backgroundBehavior
    }

    public func execute<Variables, Result>(
        _ operation: Operation<Variables, Result>,
        timeout: Duration? = nil
    ) async throws -> OperationResult<Result> {
        let requestID = UUID()
        let request = try await makeRequest(operation, id: requestID)
        let transport = self.transport
        do {
            let data = try await withTaskCancellationHandler {
                try await raceDeadline(timeout) { try await transport.execute(request) }
            } onCancel: {
                Task { await transport.cancel(requestID: requestID) }
            }
            guard data.count <= limits.responseBytes else { throw ClientFailure(.responseTooLarge) }
            return try operation.decodeResult(data)
        } catch is CancellationError {
            await transport.cancel(requestID: requestID)
            throw ClientFailure(.cancelled)
        } catch let failure as ClientFailure {
            await transport.cancel(requestID: requestID)
            throw failure
        } catch {
            await transport.cancel(requestID: requestID)
            throw ClientFailure(.transport)
        }
    }

    public func stream<Variables, Result>(
        _ operation: Operation<Variables, Result>
    ) async throws -> AsyncThrowingStream<OperationResult<Result>, any Error> {
        let requestID = UUID()
        let request = try await makeRequest(operation, id: requestID)
        let transport = self.transport
        let source: AsyncThrowingStream<Data, any Error>
        do {
            source = try await withTaskCancellationHandler {
                try await transport.stream(request)
            } onCancel: {
                Task { await transport.cancel(requestID: requestID) }
            }
        } catch is CancellationError {
            await transport.cancel(requestID: requestID)
            throw ClientFailure(.cancelled)
        } catch let failure as ClientFailure {
            await transport.cancel(requestID: requestID)
            throw failure
        } catch {
            await transport.cancel(requestID: requestID)
            throw ClientFailure(.transport)
        }
        let maximum = limits.frameBytes
        return AsyncThrowingStream { continuation in
            let producer = Task {
                var terminal = false
                do {
                    for try await frame in source {
                        try Task.checkCancellation()
                        guard frame.count <= maximum else { throw ClientFailure(.frameTooLarge) }
                        let decoded = try operation.decodeResult(frame)
                        continuation.yield(decoded)
                        if decoded.complete { terminal = true; break }
                    }
                    if terminal { continuation.finish() }
                    else { continuation.finish(throwing: ClientFailure(.streamTruncated)) }
                } catch is CancellationError {
                    continuation.finish(throwing: ClientFailure(.cancelled))
                } catch {
                    continuation.finish(throwing: error)
                }
                await transport.cancel(requestID: requestID)
            }
            continuation.onTermination = { @Sendable _ in
                producer.cancel()
                Task { await transport.cancel(requestID: requestID) }
            }
        }
    }

    public func handleBackgroundTransition() async {
        if case .cancelNetworkWork = backgroundBehavior { await transport.cancelAll() }
    }

    public func require(_ capability: UnsupportedCapability) throws -> Never {
        _ = capability
        throw ClientFailure(.unsupportedCapability)
    }

    private func makeRequest<Variables, Result>(_ operation: Operation<Variables, Result>, id: UUID) async throws -> TransportRequest {
        TransportRequest(id: id, body: try operation.canonicalRequest(), headers: try await authentication.headers(), operationKind: operation.kind)
    }
}

private func raceDeadline<Value: Sendable>(_ timeout: Duration?, operation: @escaping @Sendable () async throws -> Value) async throws -> Value {
    guard let timeout else { return try await operation() }
    return try await withThrowingTaskGroup(of: Value.self) { group in
        group.addTask { try await operation() }
        group.addTask {
            try await Task.sleep(for: timeout)
            throw ClientFailure(.deadlineExceeded)
        }
        guard let result = try await group.next() else { throw ClientFailure(.transport) }
        group.cancelAll()
        return result
    }
}

private extension URL {
    var origin: String {
        guard let scheme, let host else { return "" }
        let portPart = port.map { ":\($0)" } ?? ""
        return "\(scheme.lowercased())://\(host.lowercased())\(portPart)"
    }
}

public struct PageInfo: Codable, Equatable, Sendable {
    public let nextCursor: String?
    public let previousCursor: String?
    public let hasNextPage: Bool
    public let hasPreviousPage: Bool

    public init(nextCursor: String?, previousCursor: String?, hasNextPage: Bool, hasPreviousPage: Bool) {
        self.nextCursor = nextCursor
        self.previousCursor = previousCursor
        self.hasNextPage = hasNextPage
        self.hasPreviousPage = hasPreviousPage
    }
}

public struct Page<Element: Codable & Sendable>: Codable, Sendable {
    public let items: [Element]
    public let pageInfo: PageInfo

    public init(items: [Element], pageInfo: PageInfo) {
        self.items = items
        self.pageInfo = pageInfo
    }
}
