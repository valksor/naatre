import 'dart:async';
import 'dart:typed_data';

enum AdapterRequestKind { unary, sse, webSocket }

final class AdapterRequest {
  AdapterRequest({
    required this.kind,
    required this.endpoint,
    required this.body,
    required this.headers,
  });

  final AdapterRequestKind kind;
  final Uri endpoint;
  final Uint8List body;
  final Map<String, String> headers;
}

final class AdapterResponse {
  AdapterResponse({
    required this.status,
    required this.headers,
    required this.body,
    required Future<void> Function() close,
    this.wireBytes,
  }) : _close = close;

  final int status;
  final Map<String, String> headers;
  final Stream<List<int>> body;
  final int? wireBytes;
  final Future<void> Function() _close;
  Future<void>? _closing;

  Future<void> close() => _closing ??= _close();
}

final class AdapterExchange {
  AdapterExchange(this.response, Future<void> Function() cancel)
    : _cancel = cancel;

  final Future<AdapterResponse> response;
  final Future<void> Function() _cancel;
  Future<void>? _cancelling;

  Future<void> cancel() => _cancelling ??= _cancel();
}

abstract interface class AdapterExecutor {
  String get runtime;

  AdapterExchange start(AdapterRequest request);
}
