// ignore_for_file: deprecated_member_use, deprecated_member_use_from_same_package

import 'dart:async';
import 'dart:convert';
import 'dart:html';
import 'dart:typed_data';

import 'adapter_engine.dart';
import 'transport.dart';

AdapterExecutor createPlatformAdapterExecutor() => const _WebAdapterExecutor();

const String platformAdapterRuntime = 'browser';

final class _WebAdapterExecutor implements AdapterExecutor {
  const _WebAdapterExecutor();

  @override
  String get runtime => platformAdapterRuntime;

  @override
  AdapterExchange start(AdapterRequest request) {
    HttpRequest? activeRequest;
    WebSocket? activeSocket;
    StreamController<List<int>>? bodyController;
    bool cancelled = false;

    Future<void> cancel() async {
      if (cancelled) return;
      cancelled = true;
      activeRequest?.abort();
      activeSocket?.close(1001, 'client closed');
      final StreamController<List<int>>? controller = bodyController;
      if (controller != null && !controller.isClosed) await controller.close();
    }

    Future<AdapterResponse> execute() async {
      try {
        if (request.kind == AdapterRequestKind.webSocket) {
          final bool hasAuthentication = request.headers.keys.any(
            (String name) => !const <String>{
              'accept',
              'accept-encoding',
              'content-type',
              'naatre-timeout-ms',
            }.contains(name.toLowerCase()),
          );
          if (hasAuthentication) {
            throw const TransportFailure(
              'CLIENT_CAPABILITY_UNSUPPORTED:websocket-authentication',
            );
          }
          final Completer<AdapterResponse> ready = Completer<AdapterResponse>();
          final WebSocket socket = WebSocket(request.endpoint.toString());
          activeSocket = socket;
          final StreamController<List<int>> controller =
              StreamController<List<int>>();
          bodyController = controller;
          socket.onOpen.first.then((_) {
            if (cancelled) return;
            socket.sendString(utf8.decode(request.body, allowMalformed: false));
            ready.complete(
              AdapterResponse(
                status: 101,
                headers: const <String, String>{},
                body: controller.stream,
                close: cancel,
              ),
            );
          }).ignore();
          socket.onMessage.listen((MessageEvent event) {
            final Object? value = event.data;
            if (value case final String text) {
              controller.add(utf8.encode(text));
            } else if (value case final ByteBuffer buffer) {
              controller.add(buffer.asUint8List());
            } else {
              controller.addError(
                const TransportFailure('CLIENT_STREAM_INVALID'),
              );
            }
          });
          socket.onError.first.then((_) {
            if (!ready.isCompleted) {
              ready.completeError(
                const TransportFailure('CLIENT_TRANSPORT_ERROR'),
              );
            } else {
              controller.addError(
                const TransportFailure('CLIENT_TRANSPORT_ERROR'),
              );
            }
          }).ignore();
          socket.onClose.first.then((_) async {
            if (!ready.isCompleted) {
              ready.completeError(
                const TransportFailure('CLIENT_TRANSPORT_ERROR'),
              );
            }
            if (!controller.isClosed) await controller.close();
          }).ignore();
          return await ready.future;
        }

        final HttpRequest outgoing = HttpRequest();
        activeRequest = outgoing;
        final StreamController<List<int>> controller =
            StreamController<List<int>>();
        bodyController = controller;
        final Completer<AdapterResponse> ready = Completer<AdapterResponse>();
        int emittedCharacters = 0;
        outgoing
          ..open('POST', request.endpoint.toString())
          ..withCredentials = false;
        for (final MapEntry<String, String> header in request.headers.entries) {
          if (header.key.toLowerCase() == 'accept-encoding') continue;
          if (header.key.toLowerCase() == 'cookie') {
            throw const TransportFailure(
              'CLIENT_CAPABILITY_UNSUPPORTED:browser-cookie',
            );
          }
          outgoing.setRequestHeader(header.key, header.value);
        }
        if (request.kind == AdapterRequestKind.unary) {
          outgoing.responseType = 'arraybuffer';
        } else {
          outgoing.onReadyStateChange.listen((_) {
            if (outgoing.readyState >= HttpRequest.HEADERS_RECEIVED &&
                !ready.isCompleted) {
              ready.complete(_response(outgoing, controller, cancel));
            }
          });
          outgoing.onProgress.listen((_) {
            final String text = outgoing.responseText ?? '';
            if (text.length > emittedCharacters) {
              controller.add(utf8.encode(text.substring(emittedCharacters)));
              emittedCharacters = text.length;
            }
          });
        }
        outgoing.onLoad.first.then((_) async {
          if (request.kind == AdapterRequestKind.unary) {
            final Object? value = outgoing.response;
            if (value case final ByteBuffer buffer) {
              controller.add(buffer.asUint8List());
            } else if (value case final Uint8List bytes) {
              controller.add(bytes);
            } else {
              controller.addError(
                const TransportFailure('CLIENT_MALFORMED_RESPONSE'),
              );
            }
          } else {
            final String text = outgoing.responseText ?? '';
            if (text.length > emittedCharacters) {
              controller.add(utf8.encode(text.substring(emittedCharacters)));
            }
          }
          if (!ready.isCompleted) {
            ready.complete(_response(outgoing, controller, cancel));
          }
          if (!controller.isClosed) await controller.close();
        }).ignore();
        outgoing.onError.first.then((_) {
          final TransportFailure failure = TransportFailure(
            cancelled ? 'CLIENT_CANCELLED' : 'CLIENT_TRANSPORT_ERROR',
            retryable: !cancelled,
          );
          if (!ready.isCompleted) {
            ready.completeError(failure);
          } else {
            controller.addError(failure);
            unawaited(controller.close());
          }
        }).ignore();
        outgoing.onAbort.first.then((_) {
          final TransportFailure failure = TransportFailure(
            cancelled ? 'CLIENT_CANCELLED' : 'CLIENT_TRANSPORT_ERROR',
          );
          if (!ready.isCompleted) {
            ready.completeError(failure);
          } else {
            controller.addError(failure);
            unawaited(controller.close());
          }
        }).ignore();
        outgoing.send(request.body);
        return await ready.future;
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

AdapterResponse _response(
  HttpRequest request,
  StreamController<List<int>> controller,
  Future<void> Function() close,
) {
  final Map<String, String> headers = <String, String>{};
  request.responseHeaders.forEach((String name, String value) {
    headers[name.toLowerCase()] = value;
  });
  final String? declaredLength = request.responseHeaders['content-length'];
  return AdapterResponse(
    status: request.status ?? 0,
    headers: Map<String, String>.unmodifiable(headers),
    body: controller.stream,
    wireBytes: declaredLength == null ? null : int.tryParse(declaredLength),
    close: close,
  );
}
