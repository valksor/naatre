import 'dart:async';
import 'dart:typed_data';

import 'error.dart';

final class TransportLimits {
  const TransportLimits({
    this.responseBytes = 8 << 20,
    this.decompressedBytes = 16 << 20,
    this.frameBytes = 1 << 20,
    this.redirects = 5,
  });

  final int responseBytes;
  final int decompressedBytes;
  final int frameBytes;
  final int redirects;
}

final class CancellationToken {
  CancellationToken._(
    this._cancelled,
    this._isCancelled,
    this._register,
  );

  final Future<void> _cancelled;
  final bool Function() _isCancelled;
  final void Function() Function(void Function()) _register;

  Future<void> get cancelled => _cancelled;
  bool get isCancelled => _isCancelled();

  void Function() register(void Function() callback) => _register(callback);
}

final class CancellationController {
  final Completer<void> _completer = Completer<void>();
  final Set<void Function()> _callbacks = <void Function()>{};

  late final CancellationToken token = CancellationToken._(
    _completer.future,
    () => _completer.isCompleted,
    _register,
  );

  bool get isCancelled => _completer.isCompleted;

  void cancel() {
    if (_completer.isCompleted) return;
    _completer.complete();
    final List<void Function()> callbacks = _callbacks.toList(growable: false);
    _callbacks.clear();
    for (final void Function() callback in callbacks) {
      Future<void>.sync(callback).ignore();
    }
  }

  void Function() _register(void Function() callback) {
    if (_completer.isCompleted) {
      Future<void>.sync(callback).ignore();
      return () {};
    }
    _callbacks.add(callback);
    return () => _callbacks.remove(callback);
  }
}

final class RedirectPolicy {
  const RedirectPolicy({
    this.maximumRedirects = 5,
    this.credentialOrigins = const <String>{},
  });

  final int maximumRedirects;
  final Set<String> credentialOrigins;

  Map<String, String> headersForRedirect({
    required Uri from,
    required Uri to,
    required Map<String, String> headers,
    required int redirectCount,
  }) {
    if (redirectCount >= maximumRedirects) _fail('CLIENT_REDIRECT_LIMIT');
    final bool sameOrigin =
        from.scheme == to.scheme &&
        from.host == to.host &&
        from.port == to.port;
    if (sameOrigin || credentialOrigins.contains(to.origin)) {
      return Map<String, String>.unmodifiable(headers);
    }
    final Map<String, String> safe = <String, String>{};
    for (final MapEntry<String, String> entry in headers.entries) {
      final String name = entry.key.toLowerCase();
      if (name == 'authorization' ||
          name == 'cookie' ||
          name == 'proxy-authorization' ||
          name == 'x-naatre-tenant') {
        continue;
      }
      safe[entry.key] = entry.value;
    }
    return Map<String, String>.unmodifiable(safe);
  }
}

final class TransportContext {
  const TransportContext({
    required this.headers,
    required this.limits,
    required this.redirectPolicy,
    this.deadline,
    this.cancellation,
  });

  final Map<String, String> headers;
  final TransportLimits limits;
  final RedirectPolicy redirectPolicy;
  final DateTime? deadline;
  final CancellationToken? cancellation;
}

final class TransportResponse {
  TransportResponse({required List<int> body, int? wireBytes})
    : body = Uint8List.fromList(body),
      wireBytes = wireBytes ?? body.length;

  final Uint8List body;
  final int wireBytes;
}

final class UnaryExchange {
  UnaryExchange(this.response, Future<void> Function() cancel)
    : _cancel = cancel;

  final Future<TransportResponse> response;
  final Future<void> Function() _cancel;
  Future<void>? _closing;

  Future<void> cancel() => _closing ??= _cancel();
}

abstract interface class UnaryTransport {
  UnaryExchange send(Uint8List request, TransportContext context);
}

final class StreamConnection {
  StreamConnection(this.chunks, Future<void> Function() close) : _close = close;

  final Stream<List<int>> chunks;
  final Future<void> Function() _close;
  Future<void>? _closing;

  Future<void> close() => _closing ??= _close();
}

abstract interface class StreamTransport {
  Future<StreamConnection> open(Uint8List request, TransportContext context);
}

abstract interface class SseTransport implements StreamTransport {}

abstract interface class WebSocketTransport implements StreamTransport {}

abstract interface class AuthenticationProvider {
  Future<Map<String, String>> headers();
}

final class TransportFailure implements Exception {
  const TransportFailure(this.code, {this.retryable = false});

  final String code;
  final bool retryable;
}

Never _fail(String code, [Object? cause]) => fail(code, cause);
