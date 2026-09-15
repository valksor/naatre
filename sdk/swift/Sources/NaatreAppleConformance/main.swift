import Foundation
import NaatreApple
import NaatreCore
import NaatreGenerated

private struct CheckFailure: Error, CustomStringConvertible {
  let description: String
}

@main
struct NaatreAppleConformance {
  static func main() async throws {
    MockURLProtocol.state.reset()
    try await urlSessionUnaryAndStableRedactedFailure()
    try await urlSessionNegativeResponsesAndRedirectBoundaries()
    try await urlSessionResponseAndSSEFrameLimits()
    try await urlSessionSSESequenceAndCancellationCloseTask()
    try await lifecycleAdapterCancelsBackgroundNetworkWork()
    try exactScalarTimePresenceAndOpenVariantVectors()
    print(
      #"{"profile":"sdk.swift.apple-1","status":"passed","vectors":"urlsession-sse-cancellation-limits-scalars-time-presence-open-variants"}"#
    )
  }

  private static func urlSessionUnaryAndStableRedactedFailure() async throws {
    let expected = Data(#"{"complete":true,"data":null}"#.utf8)
    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "application/json"],
        chunks: [expected],
        finishes: true
      ))
    let transport = try URLSessionTransport(endpoint: endpoint, configuration: configuration())
    let response = try await transport.execute(request(id: UUID()))
    try check(response == expected, "unary response changed")
    let observedRequest = MockURLProtocol.state.lastRequest
    try check(observedRequest?.httpMethod == "POST", "URLSession did not use POST")
    try check(
      observedRequest?.value(forHTTPHeaderField: "Content-Type") == "application/json",
      "content type differs")

    MockURLProtocol.state.set(.failure("Bearer fixture-secret"))
    do {
      _ = try await transport.execute(request(id: UUID()))
      throw CheckFailure(description: "transport failure succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .transport, "transport failure code differs")
      let encoded = try JSONEncoder().encode(failure)
      try check(
        !String(decoding: encoded, as: UTF8.self).contains("fixture-secret"),
        "transport failure exposed credentials")
    }

    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "application/json"],
        chunks: [],
        finishes: false
      ))
    let requestID = UUID()
    let startCount = MockURLProtocol.state.startCount
    let stopCount = MockURLProtocol.state.stopCount
    let task = Task { try await transport.execute(request(id: requestID)) }
    try await waitForMockCounts(startsAbove: startCount, stopsAbove: nil)
    await transport.cancel(requestID: requestID)
    do {
      _ = try await task.value
      throw CheckFailure(description: "cancelled URLSession task succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .cancelled, "URLSession task cancellation code differs")
    }
    try await waitForMockCounts(startsAbove: startCount, stopsAbove: stopCount)
  }

  private static func urlSessionNegativeResponsesAndRedirectBoundaries() async throws {
    let transport = try URLSessionTransport(endpoint: endpoint, configuration: configuration())
    let failures: [(MockURLProtocolState.Behavior, ClientFailure.Code)] = [
      (
        .response(
          status: 503, headers: ["Content-Type": "application/json"], chunks: [Data()],
          finishes: true), .httpStatus
      ),
      (
        .response(
          status: 200, headers: ["Content-Type": "text/plain"], chunks: [Data()], finishes: true),
        .mediaTypeInvalid
      ),
    ]
    for (behavior, expectedCode) in failures {
      MockURLProtocol.state.set(behavior)
      do {
        _ = try await transport.execute(request(id: UUID()))
        throw CheckFailure(description: "negative response succeeded")
      } catch let failure as ClientFailure {
        try check(failure.code == expectedCode, "negative response code differs")
      }
    }

    let headers = [
      "Authorization": "Bearer fixture-token",
      "X-Tenant-ID": "tenant",
      "Accept": "application/json",
    ]
    let forwarded = RedirectPolicy.forwardedHeaders(
      headers,
      from: endpoint,
      to: URL(string: "https://redirect.example.test/naatre")!
    )
    try check(
      forwarded == ["Accept": "application/json"], "cross-origin redirect retained credentials")

    MockURLProtocol.state.set(
      .response(
        status: 307,
        headers: ["Content-Type": "application/json"],
        chunks: [Data()],
        finishes: true
      ))
    do {
      _ = try await transport.execute(request(id: UUID()))
      throw CheckFailure(description: "unhandled redirect succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .redirectRejected, "redirect rejection code differs")
    }

    do {
      _ = try URLSessionTransport(endpoint: URL(string: "http://api.example.test/naatre")!)
      throw CheckFailure(description: "insecure endpoint succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .endpointInvalid, "insecure endpoint code differs")
    }
  }

  private static func urlSessionResponseAndSSEFrameLimits() async throws {
    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "application/json"],
        chunks: [Data(repeating: 0x61, count: 9)],
        finishes: true
      ))
    let bounded = try URLSessionTransport(
      endpoint: endpoint,
      configuration: configuration(),
      limits: TransportLimits(responseBytes: 8, frameBytes: 8)
    )
    do {
      _ = try await bounded.execute(request(id: UUID()))
      throw CheckFailure(description: "oversized response succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .responseTooLarge, "oversized response code differs")
    }

    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "text/event-stream; charset=utf-8"],
        chunks: [Data("data: 123456789\n\n".utf8)],
        finishes: true
      ))
    let stream = try await bounded.stream(request(id: UUID(), kind: .subscription))
    do {
      for try await _ in stream {}
      throw CheckFailure(description: "oversized SSE frame succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .frameTooLarge, "oversized SSE frame code differs")
    }

    let events = String(
      repeating: "data: 1\n\n", count: URLSessionTransport.maximumBufferedFrames + 1)
    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "text/event-stream"],
        chunks: [Data(events.utf8)],
        finishes: true
      ))
    let overflowing = try await bounded.stream(request(id: UUID(), kind: .subscription))
    try await Task.sleep(for: .milliseconds(20))
    do {
      for try await _ in overflowing {}
      throw CheckFailure(description: "overflowing SSE buffer succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .streamBufferFull, "SSE buffer overflow code differs")
    }
  }

  private static func urlSessionSSESequenceAndCancellationCloseTask() async throws {
    let first = #"{"complete":false,"data":null}"#
    let terminal = #"{"complete":true,"data":null}"#
    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "text/event-stream"],
        chunks: [Data("data: \(first)\n\ndata:\ndata: \(terminal)\n\n".utf8)],
        finishes: true
      ))
    let transport = try URLSessionTransport(endpoint: endpoint, configuration: configuration())
    var frames: [Data] = []
    for try await frame in try await transport.stream(request(id: UUID(), kind: .subscription)) {
      frames.append(frame)
    }
    try check(
      frames.map { String(decoding: $0, as: UTF8.self) } == [first, "\n\(terminal)"],
      "SSE frames differ")

    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "text/event-stream"],
        chunks: [],
        finishes: false
      ))
    let requestID = UUID()
    let startCount = MockURLProtocol.state.startCount
    let stopCount = MockURLProtocol.state.stopCount
    let suspended = try await transport.stream(request(id: requestID, kind: .subscription))
    let consumer = Task { for try await _ in suspended {} }
    try await waitForMockCounts(startsAbove: startCount, stopsAbove: nil)
    await transport.cancel(requestID: requestID)
    consumer.cancel()
    try await waitForMockCounts(startsAbove: startCount, stopsAbove: stopCount)
  }

  private static func lifecycleAdapterCancelsBackgroundNetworkWork() async throws {
    MockURLProtocol.state.set(
      .response(
        status: 200,
        headers: ["Content-Type": "application/json"],
        chunks: [],
        finishes: false
      ))
    let transport = try URLSessionTransport(endpoint: endpoint, configuration: configuration())
    let client = NaatreClient(transport: transport)
    let lifecycle = AppleLifecycleAdapter(client: client)
    let startCount = MockURLProtocol.state.startCount
    let stopCount = MockURLProtocol.state.stopCount
    let operation = try makeGetAccount(GetAccountVariables(id: "acct-1"))
    let task = Task { try await client.execute(operation) }
    try await waitForMockCounts(startsAbove: startCount, stopsAbove: nil)
    await lifecycle.applicationDidEnterBackground()
    do {
      _ = try await task.value
      throw CheckFailure(description: "background-cancelled request succeeded")
    } catch let failure as ClientFailure {
      try check(failure.code == .cancelled, "background cancellation code differs")
    }
    try await waitForMockCounts(startsAbove: startCount, stopsAbove: stopCount)
  }

  private static func exactScalarTimePresenceAndOpenVariantVectors() throws {
    try check(
      try NaatreInt64(validating: "-9223372036854775808").wireValue == "-9223372036854775808",
      "Int64 vector differs")
    try check(
      try NaatreUInt64(validating: "18446744073709551615").wireValue == "18446744073709551615",
      "UInt64 vector differs")
    try check(
      try NaatreBigInt(validating: "123456789012345678901234567890").wireValue
        == "123456789012345678901234567890", "BigInt vector differs")
    try check(
      try NaatreDecimal(validating: "1234567890.123456789").wireValue == "1234567890.123456789",
      "Decimal vector differs")
    try check(
      try NaatreTimestamp(validating: "2026-09-11T23:30:01.123456789+03:00").wireValue
        == "2026-09-11T20:30:01.123456789Z", "timestamp vector differs")

    let original = NSTimeZone.default
    defer { NSTimeZone.default = original }
    for zone in ["Pacific/Kiritimati", "America/Adak", "Europe/Riga"] {
      guard let timeZone = TimeZone(identifier: zone) else {
        throw CheckFailure(description: "missing fixture time zone")
      }
      NSTimeZone.default = timeZone
      try check(
        try NaatreTimestamp(validating: "2026-09-11T23:30:01.123456789+03:00").wireValue
          == "2026-09-11T20:30:01.123456789Z", "timestamp depends on \(zone)")
    }

    let variables = GetAccountVariables(
      id: "acct-1", nickname: .null, tags: .missing, filter: .value(["role": "admin"]))
    let roundTrip = try JSONDecoder().decode(
      GetAccountVariables.self, from: JSONEncoder().encode(variables))
    guard case .null = roundTrip.nickname else {
      throw CheckFailure(description: "null presence was lost")
    }
    guard case .missing = roundTrip.tags else {
      throw CheckFailure(description: "missing presence was lost")
    }
    guard case .value(let filter) = roundTrip.filter, filter == ["role": "admin"] else {
      throw CheckFailure(description: "present value was lost")
    }

    let unknown = try JSONDecoder().decode(Status.self, from: Data(#""FUTURE""#.utf8))
    try check(unknown == .unknown("FUTURE"), "open enum value was lost")
    try check(
      try JSONEncoder().encode(unknown) == Data(#""FUTURE""#.utf8), "open enum round trip differs")
    let variant = OpenVariant(discriminator: "Future", value: .object(["field": .number(7)]))
    try check(
      try JSONDecoder().decode(OpenVariant.self, from: JSONEncoder().encode(variant)) == variant,
      "open variant round trip differs")
  }

  private static let endpoint = URL(string: "https://api.example.test/naatre")!

  private static func configuration() -> URLSessionConfiguration {
    let configuration = URLSessionConfiguration.ephemeral
    configuration.protocolClasses = [MockURLProtocol.self]
    return configuration
  }

  private static func request(id: UUID, kind: OperationKind = .query) -> TransportRequest {
    TransportRequest(
      id: id,
      body: Data(#"{"operation":"Fixture"}"#.utf8),
      headers: ["Authorization": "Bearer fixture-token", "X-Tenant-ID": "tenant"],
      operationKind: kind
    )
  }

  private static func waitForMockCounts(startsAbove: Int, stopsAbove: Int?) async throws {
    for _ in 0..<100 {
      let started = MockURLProtocol.state.startCount > startsAbove
      let stopped = stopsAbove.map { MockURLProtocol.state.stopCount > $0 } ?? true
      if started && stopped { return }
      try await Task.sleep(for: .milliseconds(5))
    }
    throw CheckFailure(description: "URLSession activity was not observed")
  }

  private static func check(_ condition: @autoclosure () throws -> Bool, _ message: String) throws {
    if try !condition() { throw CheckFailure(description: message) }
  }

}

private final class MockURLProtocol: URLProtocol, @unchecked Sendable {
  static let state = MockURLProtocolState()

  override class func canInit(with request: URLRequest) -> Bool { true }
  override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

  override func startLoading() {
    let behavior = Self.state.started(request)
    switch behavior {
    case .response(let status, let headers, let chunks, let finishes):
      let response = HTTPURLResponse(
        url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
      client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
      for chunk in chunks { client?.urlProtocol(self, didLoad: chunk) }
      if finishes { client?.urlProtocolDidFinishLoading(self) }
    case .failure(let detail):
      client?.urlProtocol(self, didFailWithError: NSError(domain: detail, code: 1))
    }
  }

  override func stopLoading() {
    Self.state.stopped()
  }
}

private final class MockURLProtocolState: @unchecked Sendable {
  enum Behavior {
    case response(status: Int, headers: [String: String], chunks: [Data], finishes: Bool)
    case failure(String)
  }

  private let lock = NSLock()
  private var behavior: Behavior = .failure("unconfigured")
  private var starts = 0
  private var stops = 0
  private var observedRequest: URLRequest?

  var startCount: Int { withLock { starts } }
  var stopCount: Int { withLock { stops } }
  var lastRequest: URLRequest? { withLock { observedRequest } }

  func reset() {
    withLock {
      behavior = .failure("unconfigured")
      starts = 0
      stops = 0
      observedRequest = nil
    }
  }

  func set(_ behavior: Behavior) {
    withLock {
      self.behavior = behavior
    }
  }

  func started(_ request: URLRequest) -> Behavior {
    withLock {
      starts += 1
      observedRequest = request
      return behavior
    }
  }

  func stopped() {
    withLock { stops += 1 }
  }

  private func withLock<Value>(_ body: () -> Value) -> Value {
    lock.lock()
    defer { lock.unlock() }
    return body()
  }
}
