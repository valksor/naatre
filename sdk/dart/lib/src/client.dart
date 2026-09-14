import 'dart:async';

import 'error.dart';
import 'model.dart';
import 'stream.dart';
import 'transport.dart';

final class RetryPolicy {
  const RetryPolicy({this.maximumAttempts = 1});

  final int maximumAttempts;
}

final class Page<T> {
  const Page({required this.items, this.nextCursor});

  final List<T> items;
  final String? nextCursor;
}

enum StreamProtocol { sse, webSocket }

final class NaatreClient {
  const NaatreClient({
    this.transport,
    this.sseTransport,
    this.webSocketTransport,
    this.authentication,
    this.retryPolicy = const RetryPolicy(),
    this.limits = const TransportLimits(),
    this.redirectPolicy = const RedirectPolicy(),
  });

  final UnaryTransport? transport;
  final SseTransport? sseTransport;
  final WebSocketTransport? webSocketTransport;
  final AuthenticationProvider? authentication;
  final RetryPolicy retryPolicy;
  final TransportLimits limits;
  final RedirectPolicy redirectPolicy;

  Future<OperationResult<TResult>> execute<TVariables, TResult>(
    Operation<TVariables, TResult> operation, {
    CancellationToken? cancellation,
    DateTime? deadline,
  }) async {
    if (retryPolicy.maximumAttempts < 1 || retryPolicy.maximumAttempts > 8) {
      _fail('CLIENT_RETRY_INVALID');
    }
    final UnaryTransport? selected = transport;
    if (selected == null) _fail('CLIENT_CAPABILITY_UNSUPPORTED:http');
    final Map<String, String> headers = await _headers();
    final TransportContext context = TransportContext(
      headers: headers,
      limits: limits,
      redirectPolicy: redirectPolicy,
      deadline: deadline,
      cancellation: cancellation,
    );
    for (int attempt = 0; attempt < retryPolicy.maximumAttempts; attempt += 1) {
      final UnaryExchange exchange = selected.send(
        operation.canonicalRequest(),
        context,
      );
      try {
        final TransportResponse response = await _wait(
          exchange,
          cancellation: cancellation,
          deadline: deadline,
        );
        if (response.wireBytes > limits.responseBytes ||
            response.body.length > limits.decompressedBytes) {
          await exchange.cancel();
          _fail('CLIENT_RESPONSE_LIMIT');
        }
        return operation.decodeResult(
          response.body,
          maximumBytes: limits.decompressedBytes,
        );
      } on TransportFailure catch (error) {
        final bool retry =
            operation.replayable &&
            error.retryable &&
            attempt + 1 < retryPolicy.maximumAttempts;
        if (!retry) rethrow;
      }
    }
    throw StateError('retry loop exhausted');
  }

  Stream<StreamFrame> stream<TVariables, TResult>(
    Operation<TVariables, TResult> operation, {
    StreamProtocol protocol = StreamProtocol.sse,
    CancellationToken? cancellation,
    DateTime? deadline,
  }) async* {
    if (protocol == StreamProtocol.webSocket) {
      _fail('CLIENT_CAPABILITY_UNSUPPORTED:websocket');
    }
    final StreamTransport? selected = sseTransport;
    if (selected == null) {
      final String capability = protocol == StreamProtocol.sse
          ? 'sse'
          : 'websocket';
      _fail('CLIENT_CAPABILITY_UNSUPPORTED:$capability');
    }
    final TransportContext context = TransportContext(
      headers: await _headers(),
      limits: limits,
      redirectPolicy: redirectPolicy,
      deadline: deadline,
      cancellation: cancellation,
    );
    final StreamConnection connection = await selected.open(
      operation.canonicalRequest(),
      context,
    );
    final Stream<List<int>> chunks = _boundedStream(
      connection,
      cancellation: cancellation,
      deadline: deadline,
    );
    yield* decodeSse(
      chunks,
      maximumFrameBytes: limits.frameBytes,
      close: connection.close,
    );
  }

  Stream<T> paginate<T>(
    Future<Page<T>> Function(String? cursor) fetch, {
    int maximumPages = 100,
  }) async* {
    if (maximumPages < 1) _fail('CLIENT_PAGINATION_INVALID');
    String? cursor;
    final Set<String> seen = <String>{};
    for (int pageNumber = 0; pageNumber < maximumPages; pageNumber += 1) {
      final Page<T> page = await fetch(cursor);
      for (final T item in page.items) {
        yield item;
      }
      final String? next = page.nextCursor;
      if (next == null) return;
      if (next.isEmpty || !seen.add(next)) _fail('CLIENT_PAGINATION_INVALID');
      cursor = next;
    }
    _fail('CLIENT_PAGINATION_LIMIT');
  }

  Future<Map<String, String>> _headers() async {
    final AuthenticationProvider? provider = authentication;
    if (provider == null) return const <String, String>{};
    return Map<String, String>.unmodifiable(await provider.headers());
  }
}

Future<TransportResponse> _wait(
  UnaryExchange exchange, {
  CancellationToken? cancellation,
  DateTime? deadline,
}) async {
  final List<Future<TransportResponse>> futures = <Future<TransportResponse>>[
    exchange.response,
  ];
  final Completer<TransportResponse> interrupted =
      Completer<TransportResponse>();
  bool active = true;
  if (cancellation != null) {
    unawaited(
      cancellation.cancelled.then((_) async {
        if (!active) return;
        try {
          await exchange.cancel();
          if (active && !interrupted.isCompleted) {
            interrupted.completeError(
              const NaatreClientException('CLIENT_CANCELLED'),
            );
          }
        } catch (error, stackTrace) {
          if (active && !interrupted.isCompleted) {
            interrupted.completeError(error, stackTrace);
          }
        }
      }),
    );
    futures.add(interrupted.future);
  }
  Timer? timer;
  if (deadline != null) {
    final Duration remaining = deadline.difference(DateTime.now().toUtc());
    if (remaining <= Duration.zero) {
      await exchange.cancel();
      _fail('CLIENT_DEADLINE_EXCEEDED');
    }
    timer = Timer(remaining, () {
      unawaited(() async {
        try {
          await exchange.cancel();
        } catch (error, stackTrace) {
          if (active && !interrupted.isCompleted) {
            interrupted.completeError(error, stackTrace);
          }
          return;
        }
        if (active && !interrupted.isCompleted) {
          interrupted.completeError(
            const NaatreClientException('CLIENT_DEADLINE_EXCEEDED'),
          );
        }
      }());
    });
    if (!futures.contains(interrupted.future)) futures.add(interrupted.future);
  }
  try {
    return await Future.any<TransportResponse>(futures);
  } finally {
    active = false;
    timer?.cancel();
  }
}

Stream<List<int>> _boundedStream(
  StreamConnection connection, {
  CancellationToken? cancellation,
  DateTime? deadline,
}) async* {
  final StreamIterator<List<int>> iterator = StreamIterator<List<int>>(
    connection.chunks,
  );
  Future<void>? closing;
  Future<void> release() => closing ??= () async {
    try {
      await iterator.cancel();
    } finally {
      await connection.close();
    }
  }();
  final Completer<void> interrupted = Completer<void>();
  String? interruptionCode;
  void interrupt(String code) {
    interruptionCode ??= code;
    if (!interrupted.isCompleted) interrupted.complete();
    release().ignore();
  }

  cancellation?.cancelled.then((_) => interrupt('CLIENT_CANCELLED')).ignore();
  Timer? timer;
  try {
    if (deadline != null) {
      final Duration remaining = deadline.difference(DateTime.now().toUtc());
      if (remaining <= Duration.zero) _fail('CLIENT_DEADLINE_EXCEEDED');
      timer = Timer(remaining, () => interrupt('CLIENT_DEADLINE_EXCEEDED'));
    }
    while (true) {
      if (cancellation?.isCancelled ?? false) _fail('CLIENT_CANCELLED');
      if (interruptionCode case final String code) _fail(code);
      final bool moved = await Future.any<bool>(<Future<bool>>[
        iterator.moveNext(),
        interrupted.future.then((_) => false),
      ]);
      if (interruptionCode case final String code) _fail(code);
      if (!moved) return;
      yield iterator.current;
    }
  } finally {
    timer?.cancel();
    await release();
  }
}

Never _fail(String code, [Object? cause]) => fail(code, cause);
