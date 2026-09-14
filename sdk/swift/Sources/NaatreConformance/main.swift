import Foundation
import NaatreCore
import NaatreGenerated

private struct CheckFailure: Error, CustomStringConvertible {
    let description: String
}

@main
struct SwiftConformance {
    static func main() async throws {
        do { try extendedScalarVectorsAreLossless() } catch { throw CheckFailure(description: "scalar vectors: \(error)") }
        do { try timestampIgnoresDeviceTimeZoneAndPreservesNanoseconds() } catch { throw CheckFailure(description: "timestamp independence: \(error)") }
        do { try generatedRequestMatchesSharedPersistedVector() } catch { throw CheckFailure(description: "persisted request: \(error)") }
        do { try missingNullPendingUnknownAndPartialErrorsRemainAccessible() } catch { throw CheckFailure(description: "selected result: \(error)") }
        do { try await cancellationAndBackgroundTransitionReleaseTransportWork() } catch { throw CheckFailure(description: "task cancellation: \(error)") }
        do { try await asyncSequenceCancellationAndTruncationReleaseTransportWork() } catch { throw CheckFailure(description: "stream cancellation: \(error)") }
        do { try await responseLimitsRedirectsAndUnsupportedCapabilities() } catch { throw CheckFailure(description: "transport boundaries: \(error)") }
        print(#"{"generatorVersion":"naatre.generator.swift-sdk-1","profile":"sdk.swift.core-1","status":"passed"}"#)
    }

    private static func extendedScalarVectorsAreLossless() throws {
        struct Fixture: Decodable { let vectors: [Vector] }
        struct Vector: Decodable { let name: String; let kind: String; let input: String; let canonical: String?; let valid: Bool }
        let fixture = try JSONDecoder().decode(Fixture.self, from: Data(contentsOf: fixtureURL("scalars.json")))
        let extended = Set(["Int64", "UInt64", "BigInt", "Decimal", "Timestamp", "Duration", "UUID", "Bytes"])
        for vector in fixture.vectors where extended.contains(vector.kind) {
            do {
                let encoded = try scalarRoundTrip(kind: vector.kind, input: Data(vector.input.utf8))
                try check(vector.valid, "\(vector.name) unexpectedly decoded")
                try check(String(decoding: encoded, as: UTF8.self) == vector.canonical, "\(vector.name) canonical output differs")
            } catch {
                if vector.valid { throw error }
            }
        }
        let url = try NaatreURL(validating: "https://example.test/a%20b?q=1")
        try check(url.url.absoluteString == url.wireValue, "URL convenience adapter changed wire bytes")
        let decimal = try NaatreDecimal(Decimal(string: "123.45", locale: Locale(identifier: "en_US_POSIX"))!)
        try check(decimal.wireValue == "123.45", "Decimal convenience adapter lost precision")
    }

    private static func timestampIgnoresDeviceTimeZoneAndPreservesNanoseconds() throws {
        let original = NSTimeZone.default
        defer { NSTimeZone.default = original }
        for zone in ["Pacific/Kiritimati", "America/Adak", "Europe/Riga"] {
            NSTimeZone.default = TimeZone(identifier: zone)!
            let timestamp = try NaatreTimestamp(validating: "2026-09-11T23:30:01.123456789+03:00")
            try check(timestamp.wireValue == "2026-09-11T20:30:01.123456789Z", "timestamp depends on device time zone")
        }
    }

    private static func generatedRequestMatchesSharedPersistedVector() throws {
        let variables = GetAccountVariables(id: "acct-1", nickname: .null, tags: .value([]), filter: .value([:]))
        let operation = try makeGetAccount(variables)
        try check(operation.persisted.digest == "cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90", "persisted digest differs")
        let expected = "{\"operation\":\"GetAccount\",\"persisted\":{\"algorithm\":\"sha-256\",\"canonicalVersion\":\"c14n-1\",\"digest\":\"cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90\"},\"variables\":{\"filter\":{},\"id\":\"acct-1\",\"nickname\":null,\"tags\":[]},\"version\":\"1\"}"
        let request = try operation.canonicalRequest()
        try check(String(decoding: request, as: UTF8.self) == expected, "canonical request differs")
    }

    private static func missingNullPendingUnknownAndPartialErrorsRemainAccessible() throws {
        let operation = try makeGetAccount(GetAccountVariables(id: "acct-1"))
        let partial = try operation.decodeResult(Data(#"{"complete":false,"data":{"profile":{"display":"Ada","nickname":null}},"errors":[{"code":"PARTIAL","message":"later failed","path":["later"]}]}"#.utf8))
        try check(!partial.complete && partial.errors.first?.code == "PARTIAL", "partial error was erased")
        guard case let .value(value) = partial.data,
              case let .value(profile) = value.profile,
              case let .value(display) = profile.display else {
            throw CheckFailure(description: "selected partial data was erased")
        }
        try check(display == "Ada", "selected display differs")
        try check(profile.nickname == .null, "null presence was erased")
        try check(value.later == .pending, "pending presence was erased")
        let unknown = try JSONDecoder().decode(Status.self, from: Data(#""FUTURE""#.utf8))
        try check(unknown == .unknown("FUTURE"), "unknown enum was erased")
        let encodedUnknown = try JSONEncoder().encode(unknown)
        try check(encodedUnknown == Data(#""FUTURE""#.utf8), "unknown enum round trip differs")
        let unknownVariant = try JSONDecoder().decode(OpenVariant.self, from: Data(#"{"$type":"Future","$value":{"field":7}}"#.utf8))
        let variantRoundTrip = try JSONDecoder().decode(OpenVariant.self, from: JSONEncoder().encode(unknownVariant))
        try check(variantRoundTrip == unknownVariant && unknownVariant.discriminator == "Future", "unknown union variant was erased")
        let roundTrip = try JSONDecoder().decode(GetAccountResultProfile.self, from: JSONEncoder().encode(profile))
        try check(roundTrip.nickname == .null && roundTrip.display == .value("Ada"), "Codable presence round trip differs")
        let variables = GetAccountVariables(id: "acct-1", nickname: .null, tags: .missing, filter: .value(["role": "admin"]))
        let decodedVariables = try JSONDecoder().decode(GetAccountVariables.self, from: JSONEncoder().encode(variables))
        try check(decodedVariables.id == "acct-1", "Codable variables lost a required value")
        guard case .null = decodedVariables.nickname,
              case .missing = decodedVariables.tags,
              case let .value(filter) = decodedVariables.filter,
              filter == ["role": "admin"] else {
            throw CheckFailure(description: "Codable variables lost presence states")
        }
    }

    private static func cancellationAndBackgroundTransitionReleaseTransportWork() async throws {
        let transport = RecordingTransport(mode: .suspended)
        let client = NaatreClient(transport: transport)
        let operation = try makeGetAccount(GetAccountVariables(id: "acct-1"))
        let task = Task { try await client.execute(operation) }
        try await Task.sleep(for: .milliseconds(20))
        task.cancel()
        do {
            _ = try await task.value
            throw CheckFailure(description: "cancelled request succeeded")
        } catch let error as ClientFailure {
            try check(error.code == .cancelled, "request cancellation returned \(error.code)")
        }
        try await eventually { await transport.cancelCount() > 0 }
        do {
            _ = try await client.execute(operation, timeout: .milliseconds(10))
            throw CheckFailure(description: "expired deadline succeeded")
        } catch let error as ClientFailure {
            try check(error.code == .deadlineExceeded, "deadline returned \(error.code)")
        }
        await client.handleBackgroundTransition()
        let cancellationCount = await transport.cancelAllCount()
        try check(cancellationCount == 1, "background transition did not cancel transport")
    }

    private static func asyncSequenceCancellationAndTruncationReleaseTransportWork() async throws {
        let establishing = RecordingTransport(mode: .suspendedStart)
        let establishingClient = NaatreClient(transport: establishing)
        let operation = try makeGetAccount(GetAccountVariables(id: "acct-1"))
        let establishment = Task { try await establishingClient.stream(operation) }
        try await Task.sleep(for: .milliseconds(20))
        establishment.cancel()
        do { _ = try await establishment.value } catch { }
        try await eventually { await establishing.cancelCount() > 0 }

        let suspended = RecordingTransport(mode: .suspended)
        let client = NaatreClient(transport: suspended)
        let stream = try await client.stream(operation)
        let consumer = Task { for try await _ in stream {} }
        try await Task.sleep(for: .milliseconds(20))
        consumer.cancel()
        do { try await consumer.value } catch { }
        try await eventually { await suspended.cancelCount() > 0 }

        let truncated = RecordingTransport(mode: .frames([Data(#"{"complete":false,"data":{"profile":{"display":"Ada"}}}"#.utf8)]))
        let truncatingClient = NaatreClient(transport: truncated)
        let truncatingStream = try await truncatingClient.stream(operation)
        do {
            for try await _ in truncatingStream {}
            throw CheckFailure(description: "truncated stream succeeded")
        } catch let error as ClientFailure {
            try check(error.code == .streamTruncated, "truncated stream returned \(error.code)")
        }
        try await eventually { await truncated.cancelCount() > 0 }
    }

    private static func responseLimitsRedirectsAndUnsupportedCapabilities() async throws {
        let operation = try makeGetAccount(GetAccountVariables(id: "acct-1"))
        let oversized = RecordingTransport(mode: .response(Data(repeating: 0x20, count: 33)))
        let client = NaatreClient(transport: oversized, limits: TransportLimits(responseBytes: 32, frameBytes: 32))
        do {
            _ = try await client.execute(operation)
            throw CheckFailure(description: "oversized response succeeded")
        } catch let error as ClientFailure {
            try check(error.code == .responseTooLarge, "oversized response returned \(error.code)")
        }
        let malformedCases: [(Data, ClientFailure.Code)] = [
            (Data(#"{"complete":"#.utf8), .resultInvalid),
            (Data(#"{"complete":true,"unknown":1}"#.utf8), .protocolInvalid),
            (Data(#"{"data":null}"#.utf8), .protocolInvalid),
        ]
        for (response, expectedCode) in malformedCases {
            let malformedClient = NaatreClient(transport: RecordingTransport(mode: .response(response)))
            do {
                _ = try await malformedClient.execute(operation)
                throw CheckFailure(description: "malformed response succeeded")
            } catch let error as ClientFailure {
                try check(error.code == expectedCode, "malformed response returned \(error.code), want \(expectedCode)")
            }
        }
        let headers = ["Authorization": "Bearer secret", "Cookie": "session", "X-Tenant-ID": "tenant", "Accept": "application/json"]
        let stripped = RedirectPolicy.forwardedHeaders(headers, from: URL(string: "https://a.example/request")!, to: URL(string: "https://b.example/request")!)
        try check(stripped == ["Accept": "application/json"], "cross-origin credentials were retained")
        let retained = RedirectPolicy.forwardedHeaders(headers, from: URL(string: "https://a.example/request")!, to: URL(string: "https://b.example/request")!, credentialOrigins: ["https://b.example"])
        try check(retained == headers, "allowlisted credentials were stripped")
        let authenticatedTransport = RecordingTransport(mode: .response(Data(#"{"complete":true,"data":{"profile":{"display":"Ada"}}}"#.utf8)))
        let authenticatedClient = NaatreClient(transport: authenticatedTransport, authentication: StaticAuthentication())
        _ = try await authenticatedClient.execute(operation)
        let authorization = await authenticatedTransport.lastHeader("Authorization")
        try check(authorization == "Bearer fixture", "authentication hook was not applied")
        do {
            try await client.require(.urlSession)
        } catch let error as ClientFailure {
            try check(error.code == .unsupportedCapability, "unsupported capability returned \(error.code)")
        }
    }

    private static func scalarRoundTrip(kind: String, input: Data) throws -> Data {
        let decoder = JSONDecoder(), encoder = JSONEncoder()
        switch kind {
        case "Int64": return try encoder.encode(decoder.decode(NaatreInt64.self, from: input))
        case "UInt64": return try encoder.encode(decoder.decode(NaatreUInt64.self, from: input))
        case "BigInt": return try encoder.encode(decoder.decode(NaatreBigInt.self, from: input))
        case "Decimal": return try encoder.encode(decoder.decode(NaatreDecimal.self, from: input))
        case "Timestamp": return try encoder.encode(decoder.decode(NaatreTimestamp.self, from: input))
        case "Duration": return try encoder.encode(decoder.decode(NaatreDuration.self, from: input))
        case "UUID": return try encoder.encode(decoder.decode(NaatreUUID.self, from: input))
        case "Bytes": return try encoder.encode(decoder.decode(NaatreBytes.self, from: input))
        default: throw ClientFailure(.unsupportedCapability)
        }
    }

    private static func fixtureURL(_ name: String) -> URL {
        URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("conformance/v1/\(name)")
    }

    private static func check(_ condition: @autoclosure () -> Bool, _ message: String) throws {
        if !condition() { throw CheckFailure(description: message) }
    }

    private static func eventually(_ predicate: @escaping @Sendable () async -> Bool) async throws {
        for _ in 0..<100 {
            if await predicate() { return }
            try await Task.sleep(for: .milliseconds(5))
        }
        throw CheckFailure(description: "condition was not observed")
    }
}

private struct StaticAuthentication: AuthenticationProvider {
    func headers() async throws -> [String: String] { ["Authorization": "Bearer fixture"] }
}

private actor RecordingTransport: ClientTransport {
    enum Mode: Sendable { case suspendedStart, suspended, response(Data), frames([Data]) }
    private let mode: Mode
    private var cancellations = 0
    private var allCancellations = 0
    private var observedHeaders: [String: String] = [:]

    init(mode: Mode) { self.mode = mode }

    func execute(_ request: TransportRequest) async throws -> Data {
        observedHeaders = request.headers
        switch mode {
        case .suspendedStart, .suspended:
            try await Task.sleep(for: .seconds(60))
            return Data()
        case let .response(data): return data
        case let .frames(frames): return frames.first ?? Data()
        }
    }

    func stream(_ request: TransportRequest) async throws -> AsyncThrowingStream<Data, any Error> {
        _ = request
        switch mode {
        case .suspendedStart:
            try await Task.sleep(for: .seconds(60))
            return AsyncThrowingStream<Data, any Error>.makeStream().stream
        case .suspended:
            return AsyncThrowingStream<Data, any Error>.makeStream().stream
        case let .response(data):
            return AsyncThrowingStream { continuation in
                continuation.yield(data)
                continuation.finish()
            }
        case let .frames(frames):
            return AsyncThrowingStream { continuation in
                for frame in frames { continuation.yield(frame) }
                continuation.finish()
            }
        }
    }

    func cancel(requestID: UUID) async { _ = requestID; cancellations += 1 }
    func cancelAll() async { allCancellations += 1 }
    func cancelCount() -> Int { cancellations }
    func cancelAllCount() -> Int { allCancellations }
    func lastHeader(_ name: String) -> String? { observedHeaders[name] }
}
