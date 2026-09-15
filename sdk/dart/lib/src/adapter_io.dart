import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'adapter_engine.dart';
import 'transport.dart';

AdapterExecutor createPlatformAdapterExecutor() => const _IoAdapterExecutor();

const String platformAdapterRuntime = 'dart-io';

final class _IoAdapterExecutor implements AdapterExecutor {
  const _IoAdapterExecutor();

  @override
  String get runtime => platformAdapterRuntime;

  @override
  AdapterExchange start(AdapterRequest request) {
    final HttpClient client = HttpClient()..autoUncompress = true;
    HttpClientRequest? activeRequest;
    WebSocket? activeSocket;
    bool cancelled = false;

    Future<void> cancel() async {
      if (cancelled) return;
      cancelled = true;
      activeRequest?.abort();
      final WebSocket? socket = activeSocket;
      if (socket != null) {
        await socket.close(WebSocketStatus.goingAway, 'client closed');
      }
      client.close(force: true);
    }

    Future<AdapterResponse> execute() async {
      try {
        if (request.kind == AdapterRequestKind.webSocket) {
          final WebSocket socket = await WebSocket.connect(
            request.endpoint.toString(),
            headers: request.headers,
          );
          activeSocket = socket;
          if (cancelled) {
            await socket.close(WebSocketStatus.goingAway, 'client closed');
            throw const TransportFailure('CLIENT_CANCELLED');
          }
          socket.add(utf8.decode(request.body, allowMalformed: false));
          return AdapterResponse(
            status: 101,
            headers: const <String, String>{},
            body: socket.map<List<int>>((Object? value) {
              if (value case final String text) return utf8.encode(text);
              if (value case final List<int> bytes) return bytes;
              throw const TransportFailure('CLIENT_STREAM_INVALID');
            }),
            close: cancel,
          );
        }
        final HttpClientRequest outgoing = await client.openUrl(
          'POST',
          request.endpoint,
        );
        activeRequest = outgoing;
        outgoing.followRedirects = false;
        request.headers.forEach(outgoing.headers.set);
        outgoing.add(request.body);
        final HttpClientResponse incoming = await outgoing.close();
        activeRequest = null;
        final Map<String, String> headers = <String, String>{};
        incoming.headers.forEach((String name, List<String> values) {
          headers[name.toLowerCase()] = values.join(',');
        });
        final int? declared = incoming.contentLength >= 0
            ? incoming.contentLength
            : null;
        return AdapterResponse(
          status: incoming.statusCode,
          headers: Map<String, String>.unmodifiable(headers),
          body: incoming,
          wireBytes: declared,
          close: cancel,
        );
      } on TransportFailure {
        rethrow;
      } on Object {
        await cancel();
        throw const TransportFailure('CLIENT_TRANSPORT_ERROR', retryable: true);
      }
    }

    return AdapterExchange(execute(), cancel);
  }
}
