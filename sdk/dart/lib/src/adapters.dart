import 'dart:async';
import 'dart:typed_data';

import 'adapter_engine.dart';
import 'adapter_stub.dart'
    if (dart.library.io) 'adapter_io.dart'
    if (dart.library.html) 'adapter_web.dart'
    as platform;
import 'error.dart';
import 'transport.dart';

const String _requestMediaType =
    'application/vnd.naatre.request+json;version=1';
const String _responseMediaType = 'application/vnd.naatre.response+json';
const String _responseAccept = '$_responseMediaType;version=1';

String get transportAdapterRuntime => platform.platformAdapterRuntime;

final class HttpTransportAdapter implements UnaryTransport {
  HttpTransportAdapter({required Uri endpoint})
    : this.withExecutor(
        endpoint: endpoint,
        executor: platform.createPlatformAdapterExecutor(),
      );

  HttpTransportAdapter.withExecutor({
    required Uri endpoint,
    required AdapterExecutor executor,
  }) : _core = _AdapterCore(endpoint, executor, AdapterRequestKind.unary);

  final _AdapterCore _core;

  @override
  UnaryExchange send(Uint8List request, TransportContext context) =>
      _core.send(request, context);
}

final class PostSseTransportAdapter implements SseTransport {
  PostSseTransportAdapter({required Uri endpoint})
    : this.withExecutor(
        endpoint: endpoint,
        executor: platform.createPlatformAdapterExecutor(),
      );

  PostSseTransportAdapter.withExecutor({
    required Uri endpoint,
    required AdapterExecutor executor,
  }) : _core = _AdapterCore(endpoint, executor, AdapterRequestKind.sse);

  final _AdapterCore _core;

  @override
  Future<StreamConnection> open(Uint8List request, TransportContext context) =>
      _core.open(request, context);
}

final class WebSocketTransportAdapter implements WebSocketTransport {
  WebSocketTransportAdapter({required Uri endpoint})
    : this.withExecutor(
        endpoint: endpoint,
        executor: platform.createPlatformAdapterExecutor(),
      );

  WebSocketTransportAdapter.withExecutor({
    required Uri endpoint,
    required AdapterExecutor executor,
  }) : _core = _AdapterCore(endpoint, executor, AdapterRequestKind.webSocket);

  final _AdapterCore _core;

  @override
  Future<StreamConnection> open(Uint8List request, TransportContext context) =>
      _core.open(request, context);
}

final class _AdapterCore {
  _AdapterCore(this.endpoint, this.executor, this.kind) {
    _validateEndpoint(endpoint, kind);
  }

  final Uri endpoint;
  final AdapterExecutor executor;
  final AdapterRequestKind kind;

  UnaryExchange send(Uint8List request, TransportContext context) {
    _validateLimits(context.limits);
    AdapterExchange? active;
    bool cancelled = false;
    Future<void> cancel() async {
      if (cancelled) return;
      cancelled = true;
      final AdapterExchange? exchange = active;
      if (exchange != null) await _cancelQuietly(exchange);
    }

    Future<TransportResponse> execute() async {
      Uri target = endpoint;
      Map<String, String> headers = _requestHeaders(context, false);
      for (int redirect = 0; ; redirect += 1) {
        if (cancelled || (context.cancellation?.isCancelled ?? false)) {
          throw const TransportFailure('CLIENT_CANCELLED');
        }
        active = executor.start(
          AdapterRequest(
            kind: kind,
            endpoint: target,
            body: Uint8List.fromList(request),
            headers: headers,
          ),
        );
        late final AdapterResponse response;
        try {
          response = await _safeResponse(active!);
        } on Object {
          await _cancelQuietly(active!);
          rethrow;
        }
        if (cancelled || (context.cancellation?.isCancelled ?? false)) {
          await _closeQuietly(response);
          throw const TransportFailure('CLIENT_CANCELLED');
        }
        if (_isRedirect(response.status)) {
          await _closeQuietly(response);
          if (redirect >= _maximumRedirects(context)) {
            throw const TransportFailure('CLIENT_REDIRECT_LIMIT');
          }
          if (response.status != 307 && response.status != 308) {
            throw const TransportFailure('CLIENT_REDIRECT_REJECTED');
          }
          final String? location = _header(response.headers, 'location');
          final Uri next = _redirectTarget(target, location);
          headers = context.redirectPolicy.headersForRedirect(
            from: target,
            to: next,
            headers: headers,
            redirectCount: redirect,
          );
          target = next;
          continue;
        }
        try {
          _validateUnaryResponse(response, context.limits);
          final Uint8List body = await _readBounded(
            response.body,
            context.limits.decompressedBytes,
          );
          if (response.status < 200 || response.status >= 300) {
            throw const TransportFailure('CLIENT_REMOTE_ERROR');
          }
          return TransportResponse(
            body: body,
            wireBytes: response.wireBytes ?? body.length,
          );
        } finally {
          await _closeQuietly(response);
        }
      }
    }

    return UnaryExchange(execute(), cancel);
  }

  Future<StreamConnection> open(
    Uint8List request,
    TransportContext context,
  ) async {
    _validateLimits(context.limits);
    Uri target = endpoint;
    Map<String, String> headers = _requestHeaders(
      context,
      kind == AdapterRequestKind.sse,
    );
    late AdapterExchange exchange;
    late AdapterResponse response;
    for (int redirect = 0; ; redirect += 1) {
      exchange = executor.start(
        AdapterRequest(
          kind: kind,
          endpoint: target,
          body: Uint8List.fromList(request),
          headers: headers,
        ),
      );
      try {
        response = await _waitForOpen(exchange, context);
      } on Object {
        await _cancelQuietly(exchange);
        rethrow;
      }
      if (kind == AdapterRequestKind.webSocket ||
          !_isRedirect(response.status)) {
        break;
      }
      await _closeQuietly(response);
      await _cancelQuietly(exchange);
      if (redirect >= _maximumRedirects(context)) {
        throw const TransportFailure('CLIENT_REDIRECT_LIMIT');
      }
      if (response.status != 307 && response.status != 308) {
        throw const TransportFailure('CLIENT_REDIRECT_REJECTED');
      }
      final Uri next = _redirectTarget(
        target,
        _header(response.headers, 'location'),
      );
      headers = context.redirectPolicy.headersForRedirect(
        from: target,
        to: next,
        headers: headers,
        redirectCount: redirect,
      );
      target = next;
    }
    if (kind != AdapterRequestKind.webSocket) {
      try {
        _validateSseResponse(response, context.limits);
      } on Object {
        await _closeQuietly(response);
        await _cancelQuietly(exchange);
        rethrow;
      }
    }
    bool closed = false;
    Future<void> close() async {
      if (closed) return;
      closed = true;
      await _closeQuietly(response);
      await _cancelQuietly(exchange);
    }

    return StreamConnection(
      _boundedChunks(
        response.body,
        frameBytes: kind == AdapterRequestKind.webSocket
            ? context.limits.frameBytes
            : null,
        totalBytes: kind == AdapterRequestKind.sse
            ? context.limits.decompressedBytes
            : null,
      ),
      close,
    );
  }
}

Future<AdapterResponse> _safeResponse(AdapterExchange exchange) async {
  try {
    return await exchange.response;
  } on TransportFailure {
    rethrow;
  } on NaatreClientException catch (error) {
    throw TransportFailure(error.code);
  } on Object {
    throw const TransportFailure('CLIENT_TRANSPORT_ERROR', retryable: true);
  }
}

Future<AdapterResponse> _waitForOpen(
  AdapterExchange exchange,
  TransportContext context,
) async {
  final Completer<AdapterResponse> interrupted = Completer<AdapterResponse>();
  bool active = true;
  Future<void> interrupt(String code) async {
    if (!active || interrupted.isCompleted) return;
    try {
      await exchange.cancel();
    } on Object {
      // Cancellation failures are hidden behind the stable public code.
    }
    if (active && !interrupted.isCompleted) {
      interrupted.completeError(NaatreClientException(code));
    }
  }

  final void Function()? unregister = context.cancellation?.register(
    () => unawaited(interrupt('CLIENT_CANCELLED')),
  );
  Timer? timer;
  final DateTime? deadline = context.deadline;
  if (deadline != null) {
    final Duration remaining = deadline.difference(DateTime.now().toUtc());
    if (remaining <= Duration.zero) {
      await interrupt('CLIENT_DEADLINE_EXCEEDED');
    } else {
      timer = Timer(
        remaining,
        () => unawaited(interrupt('CLIENT_DEADLINE_EXCEEDED')),
      );
    }
  }
  try {
    return await Future.any(<Future<AdapterResponse>>[
      _safeResponse(exchange),
      interrupted.future,
    ]);
  } finally {
    active = false;
    timer?.cancel();
    unregister?.call();
  }
}

Map<String, String> _requestHeaders(TransportContext context, bool stream) {
  final Map<String, String> headers = <String, String>{};
  for (final MapEntry<String, String> entry in context.headers.entries) {
    final String name = entry.key.toLowerCase();
    if (!_validHeaderName(name) || !_validHeaderValue(entry.value)) {
      throw const TransportFailure('CLIENT_AUTHENTICATION_FAILED');
    }
    if (const <String>{
      'accept',
      'accept-encoding',
      'content-length',
      'content-type',
      'host',
      'last-event-id',
      'naatre-principal',
    }.contains(name)) {
      throw const TransportFailure('CLIENT_AUTHENTICATION_FAILED');
    }
    headers[name] = entry.value;
  }
  headers['accept'] = stream ? 'text/event-stream' : _responseAccept;
  headers['accept-encoding'] = stream ? 'identity' : 'gzip';
  headers['content-type'] = _requestMediaType;
  final DateTime? deadline = context.deadline;
  if (deadline != null) {
    final int milliseconds = deadline
        .difference(DateTime.now().toUtc())
        .inMilliseconds;
    if (milliseconds <= 0) {
      throw const TransportFailure('CLIENT_DEADLINE_EXCEEDED');
    }
    headers['naatre-timeout-ms'] = milliseconds.toString();
  }
  return Map<String, String>.unmodifiable(headers);
}

void _validateUnaryResponse(AdapterResponse response, TransportLimits limits) {
  final String? contentType = _header(response.headers, 'content-type');
  if (!_mediaType(contentType, _responseMediaType, requireVersion: true)) {
    throw const TransportFailure('CLIENT_UNSUPPORTED_MEDIA_TYPE');
  }
  final String encoding =
      (_header(response.headers, 'content-encoding') ?? 'identity')
          .trim()
          .toLowerCase();
  if (encoding != 'identity' && encoding != 'gzip') {
    throw const TransportFailure('CLIENT_UNSUPPORTED_ENCODING');
  }
  _validateWireBytes(response, limits.responseBytes);
}

void _validateSseResponse(AdapterResponse response, TransportLimits limits) {
  if (response.status < 200 || response.status >= 300) {
    throw const TransportFailure('CLIENT_REMOTE_ERROR');
  }
  final String? contentType = _header(response.headers, 'content-type');
  if (!_mediaType(contentType, 'text/event-stream')) {
    throw const TransportFailure('CLIENT_UNSUPPORTED_MEDIA_TYPE');
  }
  final String encoding =
      (_header(response.headers, 'content-encoding') ?? 'identity')
          .trim()
          .toLowerCase();
  if (encoding != 'identity') {
    throw const TransportFailure('CLIENT_UNSUPPORTED_ENCODING');
  }
  _validateWireBytes(response, limits.responseBytes);
}

void _validateWireBytes(AdapterResponse response, int maximum) {
  final int? declared = response.wireBytes;
  if (declared != null && (declared < 0 || declared > maximum)) {
    throw const TransportFailure('CLIENT_RESPONSE_LIMIT');
  }
  final String? contentLength = _header(response.headers, 'content-length');
  if (contentLength != null) {
    final int? parsed = int.tryParse(contentLength);
    if (parsed == null || parsed < 0) {
      throw const TransportFailure('CLIENT_MALFORMED_RESPONSE');
    }
    if (parsed > maximum) {
      throw const TransportFailure('CLIENT_RESPONSE_LIMIT');
    }
  }
}

Future<Uint8List> _readBounded(Stream<List<int>> source, int maximum) async {
  final BytesBuilder builder = BytesBuilder(copy: false);
  int received = 0;
  try {
    await for (final List<int> chunk in source) {
      received += chunk.length;
      if (received > maximum) {
        throw const TransportFailure('CLIENT_RESPONSE_LIMIT');
      }
      builder.add(chunk);
    }
  } on TransportFailure {
    rethrow;
  } on Object {
    throw const TransportFailure('CLIENT_TRANSPORT_ERROR', retryable: true);
  }
  return builder.takeBytes();
}

Stream<List<int>> _boundedChunks(
  Stream<List<int>> source, {
  required int? frameBytes,
  required int? totalBytes,
}) async* {
  int received = 0;
  try {
    await for (final List<int> chunk in source) {
      if (frameBytes != null && chunk.length > frameBytes) {
        throw const TransportFailure('CLIENT_STREAM_LIMIT');
      }
      received += chunk.length;
      if (totalBytes != null && received > totalBytes) {
        throw const TransportFailure('CLIENT_RESPONSE_LIMIT');
      }
      yield chunk;
    }
  } on TransportFailure {
    rethrow;
  } on NaatreClientException {
    rethrow;
  } on Object {
    throw const TransportFailure('CLIENT_TRANSPORT_ERROR');
  }
}

Future<void> _closeQuietly(AdapterResponse response) async {
  try {
    await response.close();
  } on Object {
    // Resource cleanup is best-effort and must not expose implementation data.
  }
}

Future<void> _cancelQuietly(AdapterExchange exchange) async {
  try {
    await exchange.cancel();
  } on Object {
    // Resource cleanup is best-effort and must not expose implementation data.
  }
}

bool _isRedirect(int status) =>
    const <int>{301, 302, 303, 307, 308}.contains(status);

Uri _redirectTarget(Uri current, String? location) {
  if (location == null || location.isEmpty) {
    throw const TransportFailure('CLIENT_REDIRECT_REJECTED');
  }
  final Uri next;
  try {
    next = current.resolve(location);
  } on FormatException {
    throw const TransportFailure('CLIENT_REDIRECT_REJECTED');
  }
  _validateEndpoint(next, AdapterRequestKind.unary);
  return next;
}

void _validateEndpoint(Uri endpoint, AdapterRequestKind kind) {
  final bool validScheme = kind == AdapterRequestKind.webSocket
      ? endpoint.scheme == 'wss'
      : endpoint.scheme == 'http' || endpoint.scheme == 'https';
  if (!validScheme ||
      endpoint.host.isEmpty ||
      endpoint.userInfo.isNotEmpty ||
      endpoint.hasFragment) {
    throw const NaatreClientException('CLIENT_CONFIG_INVALID');
  }
}

void _validateLimits(TransportLimits limits) {
  if (limits.responseBytes < 1 ||
      limits.decompressedBytes < 1 ||
      limits.frameBytes < 1 ||
      limits.redirects < 0) {
    throw const TransportFailure('CLIENT_CONFIG_INVALID');
  }
}

int _maximumRedirects(TransportContext context) {
  final int policy = context.redirectPolicy.maximumRedirects;
  if (policy < 0) {
    throw const TransportFailure('CLIENT_CONFIG_INVALID');
  }
  return policy < context.limits.redirects ? policy : context.limits.redirects;
}

String? _header(Map<String, String> headers, String name) {
  for (final MapEntry<String, String> entry in headers.entries) {
    if (entry.key.toLowerCase() == name) return entry.value;
  }
  return null;
}

bool _mediaType(String? value, String expected, {bool requireVersion = false}) {
  if (value == null) return false;
  final List<String> parts = value
      .toLowerCase()
      .split(';')
      .map((String part) => part.trim())
      .toList(growable: false);
  if (parts.first != expected) return false;
  final Map<String, String> parameters = <String, String>{};
  for (final String part in parts.skip(1)) {
    final int separator = part.indexOf('=');
    if (separator < 1) return false;
    final String name = part.substring(0, separator);
    String value = part.substring(separator + 1);
    if (value.length >= 2 && value.startsWith('"') && value.endsWith('"')) {
      value = value.substring(1, value.length - 1);
    }
    if (parameters.containsKey(name)) return false;
    parameters[name] = value;
  }
  if (requireVersion && parameters['version'] != '1') return false;
  final String? charset = parameters['charset'];
  return charset == null || charset == 'utf-8';
}

bool _validHeaderName(String value) =>
    value.isNotEmpty && RegExp(r"^[!#$%&'*+.^_`|~0-9a-z-]+$").hasMatch(value);

bool _validHeaderValue(String value) => !value.codeUnits.any(
  (int unit) => unit < 0x20 && unit != 0x09 || unit == 0x7f,
);
