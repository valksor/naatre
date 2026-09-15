import Foundation
import NaatreCore

public actor URLSessionTransport: ClientTransport {
  public static let maximumBufferedFrames = 16
  public let endpoint: URL
  public let limits: TransportLimits

  private struct CancellationHandle: Sendable {
    let cancel: @Sendable () -> Void
  }

  private let session: URLSession
  private let redirectDelegate: RedirectDelegate
  private var active: [UUID: CancellationHandle] = [:]

  public init(
    endpoint: URL,
    configuration: URLSessionConfiguration = .ephemeral,
    limits: TransportLimits = TransportLimits(),
    credentialOrigins: Set<String> = []
  ) throws {
    guard endpoint.scheme?.lowercased() == "https", endpoint.host != nil else {
      throw ClientFailure(.endpointInvalid)
    }
    guard limits.responseBytes > 0, limits.frameBytes > 0, limits.redirects >= 0 else {
      throw ClientFailure(.protocolInvalid)
    }
    guard let isolatedConfiguration = configuration.copy() as? URLSessionConfiguration else {
      throw ClientFailure(.transport)
    }
    isolatedConfiguration.httpShouldSetCookies = false
    isolatedConfiguration.httpCookieStorage = nil
    isolatedConfiguration.requestCachePolicy = .reloadIgnoringLocalCacheData
    isolatedConfiguration.urlCache = nil
    let redirectDelegate = RedirectDelegate(
      limit: limits.redirects, credentialOrigins: credentialOrigins)
    self.endpoint = endpoint
    self.limits = limits
    self.redirectDelegate = redirectDelegate
    session = URLSession(
      configuration: isolatedConfiguration, delegate: redirectDelegate, delegateQueue: nil)
  }

  deinit {
    session.invalidateAndCancel()
  }

  public func execute(_ request: TransportRequest) async throws -> Data {
    guard active[request.id] == nil else { throw ClientFailure(.protocolInvalid) }
    let session = self.session
    let endpoint = self.endpoint
    let maximum = limits.responseBytes
    let worker = Task {
      try await Self.readUnary(
        session: session,
        request: Self.makeRequest(request, endpoint: endpoint, accept: "application/json"),
        maximumBytes: maximum
      )
    }
    active[request.id] = CancellationHandle(cancel: { worker.cancel() })
    defer { active.removeValue(forKey: request.id) }
    do {
      return try await withTaskCancellationHandler {
        try await worker.value
      } onCancel: {
        worker.cancel()
      }
    } catch {
      throw Self.publicFailure(error)
    }
  }

  public func stream(_ request: TransportRequest) async throws -> AsyncThrowingStream<
    Data, any Error
  > {
    guard active[request.id] == nil else { throw ClientFailure(.protocolInvalid) }
    let session = self.session
    let endpoint = self.endpoint
    let maximum = limits.frameBytes
    let pair = AsyncThrowingStream<Data, any Error>.makeStream(
      bufferingPolicy: .bufferingOldest(Self.maximumBufferedFrames))
    let worker = Task {
      do {
        try await Self.readSSE(
          session: session,
          request: Self.makeRequest(request, endpoint: endpoint, accept: "text/event-stream"),
          maximumFrameBytes: maximum,
          continuation: pair.continuation
        )
        pair.continuation.finish()
      } catch {
        pair.continuation.finish(throwing: Self.publicFailure(error))
      }
      self.finished(request.id)
    }
    active[request.id] = CancellationHandle(cancel: { worker.cancel() })
    pair.continuation.onTermination = { @Sendable _ in
      worker.cancel()
      Task { await self.finished(request.id) }
    }
    return pair.stream
  }

  public func cancel(requestID: UUID) async {
    active[requestID]?.cancel()
  }

  public func cancelAll() async {
    let handles = Array(active.values)
    for handle in handles { handle.cancel() }
  }

  private func finished(_ requestID: UUID) {
    active.removeValue(forKey: requestID)
  }

  private static func makeRequest(_ request: TransportRequest, endpoint: URL, accept: String) throws
    -> URLRequest
  {
    for (name, value) in request.headers {
      guard !name.isEmpty,
        !name.contains("\r"), !name.contains("\n"),
        !value.contains("\r"), !value.contains("\n")
      else {
        throw ClientFailure(.protocolInvalid)
      }
    }
    var urlRequest = URLRequest(url: endpoint)
    urlRequest.httpMethod = "POST"
    urlRequest.httpBody = request.body
    for (name, value) in request.headers { urlRequest.setValue(value, forHTTPHeaderField: name) }
    urlRequest.setValue("application/json", forHTTPHeaderField: "Content-Type")
    urlRequest.setValue(accept, forHTTPHeaderField: "Accept")
    urlRequest.setValue("no-store", forHTTPHeaderField: "Cache-Control")
    return urlRequest
  }

  private static func readUnary(session: URLSession, request: URLRequest, maximumBytes: Int)
    async throws -> Data
  {
    let (bytes, response) = try await session.bytes(for: request)
    try validate(response, mediaType: "application/json")
    if response.expectedContentLength > maximumBytes {
      throw ClientFailure(.responseTooLarge)
    }
    var data = Data()
    data.reserveCapacity(min(maximumBytes, max(0, Int(response.expectedContentLength))))
    for try await byte in bytes {
      try Task.checkCancellation()
      guard data.count < maximumBytes else { throw ClientFailure(.responseTooLarge) }
      data.append(byte)
    }
    return data
  }

  private static func readSSE(
    session: URLSession,
    request: URLRequest,
    maximumFrameBytes: Int,
    continuation: AsyncThrowingStream<Data, any Error>.Continuation
  ) async throws {
    let (bytes, response) = try await session.bytes(for: request)
    try validate(response, mediaType: "text/event-stream")
    var line = Data()
    var event = Data()
    var dataLineCount = 0
    for try await byte in bytes {
      try Task.checkCancellation()
      if byte == 0x0A {
        if line.last == 0x0D { line.removeLast() }
        if let frame = try consumeSSELine(
          line,
          event: &event,
          dataLineCount: &dataLineCount,
          maximumFrameBytes: maximumFrameBytes
        ) {
          switch continuation.yield(frame) {
          case .enqueued:
            break
          case .dropped:
            throw ClientFailure(.streamBufferFull)
          case .terminated:
            throw CancellationError()
          @unknown default:
            throw ClientFailure(.transport)
          }
        }
        line.removeAll(keepingCapacity: true)
      } else {
        guard line.count <= maximumFrameBytes + 6 else { throw ClientFailure(.frameTooLarge) }
        line.append(byte)
      }
    }
  }

  private static func consumeSSELine(
    _ line: Data,
    event: inout Data,
    dataLineCount: inout Int,
    maximumFrameBytes: Int
  ) throws -> Data? {
    if line.isEmpty {
      guard dataLineCount > 0 else { return nil }
      let frame = event
      event.removeAll(keepingCapacity: true)
      dataLineCount = 0
      return frame
    }
    if line.first == 0x3A { return nil }
    let prefix = Data("data:".utf8)
    guard line.starts(with: prefix) else { return nil }
    var start = prefix.count
    if line.count > start, line[line.index(line.startIndex, offsetBy: start)] == 0x20 { start += 1 }
    let value = line.suffix(from: line.index(line.startIndex, offsetBy: start))
    let separator = dataLineCount == 0 ? 0 : 1
    guard event.count <= maximumFrameBytes - separator,
      value.count <= maximumFrameBytes - event.count - separator
    else {
      throw ClientFailure(.frameTooLarge)
    }
    if separator == 1 { event.append(0x0A) }
    event.append(contentsOf: value)
    dataLineCount += 1
    return nil
  }

  private static func validate(_ response: URLResponse, mediaType: String) throws {
    guard let response = response as? HTTPURLResponse else { throw ClientFailure(.responseInvalid) }
    guard (200..<300).contains(response.statusCode) else {
      if (300..<400).contains(response.statusCode) { throw ClientFailure(.redirectRejected) }
      throw ClientFailure(.httpStatus)
    }
    guard response.mimeType?.lowercased() == mediaType else {
      throw ClientFailure(.mediaTypeInvalid)
    }
  }

  private static func publicFailure(_ error: any Error) -> ClientFailure {
    if let failure = error as? ClientFailure { return failure }
    if error is CancellationError { return ClientFailure(.cancelled) }
    if let urlError = error as? URLError, urlError.code == .cancelled {
      return ClientFailure(.cancelled)
    }
    return ClientFailure(.transport)
  }
}

public struct AppleLifecycleAdapter: Sendable {
  private let client: NaatreClient

  public init(client: NaatreClient) {
    self.client = client
  }

  public func applicationDidEnterBackground() async {
    await client.handleBackgroundTransition()
  }
}

private final class RedirectDelegate: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
  private let lock = NSLock()
  private let limit: Int
  private let credentialOrigins: Set<String>
  private var counts: [Int: Int] = [:]

  init(limit: Int, credentialOrigins: Set<String>) {
    self.limit = limit
    self.credentialOrigins = credentialOrigins
  }

  func urlSession(
    _ session: URLSession,
    task: URLSessionTask,
    willPerformHTTPRedirection response: HTTPURLResponse,
    newRequest request: URLRequest,
    completionHandler: @escaping @Sendable (URLRequest?) -> Void
  ) {
    _ = session
    guard response.statusCode == 307 || response.statusCode == 308,
      let source = task.currentRequest,
      let sourceURL = source.url,
      let destinationURL = request.url,
      request.httpMethod == "POST",
      destinationURL.scheme?.lowercased() == "https",
      acceptRedirect(taskIdentifier: task.taskIdentifier)
    else {
      completionHandler(nil)
      return
    }
    var redirected = request
    redirected.allHTTPHeaderFields = RedirectPolicy.forwardedHeaders(
      source.allHTTPHeaderFields ?? [:],
      from: sourceURL,
      to: destinationURL,
      credentialOrigins: credentialOrigins
    )
    completionHandler(redirected)
  }

  func urlSession(
    _ session: URLSession, task: URLSessionTask, didCompleteWithError error: (any Error)?
  ) {
    _ = session
    _ = error
    lock.lock()
    counts.removeValue(forKey: task.taskIdentifier)
    lock.unlock()
  }

  private func acceptRedirect(taskIdentifier: Int) -> Bool {
    lock.lock()
    defer { lock.unlock() }
    let count = counts[taskIdentifier, default: 0]
    guard count < limit else { return false }
    counts[taskIdentifier] = count + 1
    return true
  }
}
