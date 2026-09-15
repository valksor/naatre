import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:naatre/generated.dart';
import 'package:naatre/naatre.dart';
import 'package:naatre/src/adapter_engine.dart';

import 'support.dart';

final class _FixtureExecutor implements AdapterExecutor {
  final List<AdapterRequest> requests = <AdapterRequest>[];
  final List<AdapterResponse Function(AdapterRequest)> responses =
      <AdapterResponse Function(AdapterRequest)>[];
  int cancellations = 0;

  @override
  String get runtime => 'fixture';

  @override
  AdapterExchange start(AdapterRequest request) {
    requests.add(request);
    final AdapterResponse Function(AdapterRequest) response = responses
        .removeAt(0);
    return AdapterExchange(
      Future<AdapterResponse>.sync(() => response(request)),
      () async => cancellations += 1,
    );
  }
}

final class _BlockingExecutor implements AdapterExecutor {
  final Completer<AdapterResponse> response = Completer<AdapterResponse>();
  int cancellations = 0;

  @override
  String get runtime => 'blocking-fixture';

  @override
  AdapterExchange start(AdapterRequest request) =>
      AdapterExchange(response.future, () async => cancellations += 1);
}

final class _RotatingAuthentication implements AuthenticationProvider {
  int calls = 0;

  @override
  Future<Map<String, String>> headers() async {
    calls += 1;
    return <String, String>{
      'Authorization': 'Bearer secret-$calls',
      'Cookie': 'session=secret-$calls',
      'Naatre-Tenant': 'tenant-$calls',
      'X-CSRF-Token': 'csrf-$calls',
      'X-Request': 'request-$calls',
    };
  }
}

final class _FailingAuthentication implements AuthenticationProvider {
  @override
  Future<Map<String, String>> headers() {
    throw StateError('Bearer private-secret');
  }
}

Future<void> main() async {
  await _unaryAndWarmState();
  await _redirectAndFailureRedaction();
  await _cancellationAndLimits();
  await _sseAndWebSocket();
  await _streamCancellationAndTotalLimit();
  _runtimeAndNumericBoundaries();
}

Future<void> _unaryAndWarmState() async {
  final _FixtureExecutor executor = _FixtureExecutor();
  int closes = 0;
  for (int index = 0; index < 2; index += 1) {
    executor.responses.add(
      (_) => AdapterResponse(
        status: 200,
        headers: const <String, String>{
          'content-type': 'application/vnd.naatre.response+json;version=1',
          'content-encoding': 'identity',
        },
        body: Stream<List<int>>.value(
          utf8.encode('{"complete":true,"data":{},"errors":[]}'),
        ),
        close: () async => closes += 1,
      ),
    );
  }
  final _RotatingAuthentication authentication = _RotatingAuthentication();
  final NaatreClient client = NaatreClient(
    transport: HttpTransportAdapter.withExecutor(
      endpoint: Uri.parse('https://api.example/v1/execute'),
      executor: executor,
    ),
    authentication: authentication,
  );
  final Operation<GetAccountVariables, GetAccountResult> operation = getAccount(
    const GetAccountVariables(id: 'acct-1'),
  );
  await client.execute(operation);
  await client.execute(operation);
  check(authentication.calls == 2, 'authentication was not operation-local');
  check(executor.requests.length == 2, 'warm adapter lost an operation');
  check(
    executor.requests[0].headers['authorization'] == 'Bearer secret-1' &&
        executor.requests[1].headers['authorization'] == 'Bearer secret-2',
    'warm adapter leaked authentication state',
  );
  check(
    utf8.decode(executor.requests.first.body) ==
        '{"operation":"GetAccount","persisted":{"algorithm":"sha-256","canonicalVersion":"c14n-1","digest":"cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90"},"variables":{"id":"acct-1"},"version":"1"}',
    'adapter request or shared persisted hash drifted',
  );
  check(closes == 2, 'unary responses were not released');
}

Future<void> _redirectAndFailureRedaction() async {
  final _FixtureExecutor executor = _FixtureExecutor();
  executor.responses
    ..add(
      (_) => AdapterResponse(
        status: 307,
        headers: const <String, String>{
          'location': 'https://redirect.example/v1/execute',
        },
        body: const Stream<List<int>>.empty(),
        close: () async {},
      ),
    )
    ..add(
      (_) => AdapterResponse(
        status: 200,
        headers: const <String, String>{
          'content-type': 'application/vnd.naatre.response+json;version=1',
        },
        body: Stream<List<int>>.value(
          utf8.encode('{"complete":true,"data":{},"errors":[]}'),
        ),
        close: () async {},
      ),
    );
  await NaatreClient(
    transport: HttpTransportAdapter.withExecutor(
      endpoint: Uri.parse('https://api.example/v1/execute'),
      executor: executor,
    ),
    authentication: _RotatingAuthentication(),
  ).execute(getAccount(const GetAccountVariables(id: 'acct-1')));
  check(
    !executor.requests[1].headers.containsKey('authorization') &&
        !executor.requests[1].headers.containsKey('cookie') &&
        !executor.requests[1].headers.containsKey('naatre-tenant') &&
        !executor.requests[1].headers.containsKey('x-csrf-token'),
    'cross-origin redirect exposed credentials or protected metadata',
  );
  check(
    executor.requests[1].headers['x-request'] == 'request-1',
    'redirect removed non-sensitive metadata',
  );

  final _FixtureExecutor blockedRedirect = _FixtureExecutor();
  blockedRedirect.responses.add(
    (_) => AdapterResponse(
      status: 307,
      headers: const <String, String>{
        'location': 'https://redirect.example/v1/execute',
      },
      body: const Stream<List<int>>.empty(),
      close: () async {},
    ),
  );
  await _expectTransportFailure('CLIENT_REDIRECT_LIMIT', () async {
    await NaatreClient(
      transport: HttpTransportAdapter.withExecutor(
        endpoint: Uri.parse('https://api.example/v1/execute'),
        executor: blockedRedirect,
      ),
      limits: const TransportLimits(redirects: 0),
    ).execute(getAccount(const GetAccountVariables(id: 'acct-1')));
  });

  final _FixtureExecutor invalid = _FixtureExecutor();
  invalid.responses.add(
    (_) => AdapterResponse(
      status: 200,
      headers: const <String, String>{'content-type': 'text/plain'},
      body: Stream<List<int>>.value(utf8.encode('Bearer private-secret')),
      close: () async {},
    ),
  );
  await _expectTransportFailure('CLIENT_UNSUPPORTED_MEDIA_TYPE', () async {
    await NaatreClient(
      transport: HttpTransportAdapter.withExecutor(
        endpoint: Uri.parse('https://api.example/v1/execute'),
        executor: invalid,
      ),
    ).execute(getAccount(const GetAccountVariables(id: 'acct-1')));
  });
  const NaatreClientException publicError = NaatreClientException(
    'CLIENT_TRANSPORT_ERROR',
    'Bearer private-secret',
  );
  check(
    !publicError.toString().contains('private-secret'),
    'public exception exposed an implementation detail',
  );
  await expectAsyncClientError('CLIENT_AUTHENTICATION_FAILED', () async {
    await NaatreClient(
      transport: HttpTransportAdapter.withExecutor(
        endpoint: Uri.parse('https://api.example/v1/execute'),
        executor: _FixtureExecutor(),
      ),
      authentication: _FailingAuthentication(),
    ).execute(getAccount(const GetAccountVariables(id: 'acct-1')));
  });

  final _FixtureExecutor bodyFailure = _FixtureExecutor();
  bodyFailure.responses.add(
    (_) => AdapterResponse(
      status: 200,
      headers: const <String, String>{
        'content-type': 'application/vnd.naatre.response+json;version=1',
      },
      body: Stream<List<int>>.error(StateError('Bearer private-secret')),
      close: () async => throw StateError('Bearer private-secret'),
    ),
  );
  await _expectTransportFailure('CLIENT_TRANSPORT_ERROR', () async {
    await NaatreClient(
      transport: HttpTransportAdapter.withExecutor(
        endpoint: Uri.parse('https://api.example/v1/execute'),
        executor: bodyFailure,
      ),
    ).execute(getAccount(const GetAccountVariables(id: 'acct-1')));
  });
}

Future<void> _cancellationAndLimits() async {
  final _BlockingExecutor blocking = _BlockingExecutor();
  final CancellationController cancellation = CancellationController();
  final Future<OperationResult<GetAccountResult>> pending =
      NaatreClient(
        transport: HttpTransportAdapter.withExecutor(
          endpoint: Uri.parse('https://api.example/v1/execute'),
          executor: blocking,
        ),
      ).execute(
        getAccount(const GetAccountVariables(id: 'acct-1')),
        cancellation: cancellation.token,
      );
  await Future<void>.delayed(Duration.zero);
  cancellation.cancel();
  await expectAsyncClientError('CLIENT_CANCELLED', () async => pending);
  check(blocking.cancellations == 1, 'unary cancellation did not close I/O');

  final _FixtureExecutor limited = _FixtureExecutor();
  limited.responses.add(
    (_) => AdapterResponse(
      status: 200,
      headers: const <String, String>{
        'content-type': 'application/vnd.naatre.response+json;version=1',
        'content-length': '5',
      },
      body: Stream<List<int>>.value(const <int>[1, 2, 3, 4, 5]),
      close: () async {},
      wireBytes: 5,
    ),
  );
  await _expectTransportFailure('CLIENT_RESPONSE_LIMIT', () async {
    await NaatreClient(
      transport: HttpTransportAdapter.withExecutor(
        endpoint: Uri.parse('https://api.example/v1/execute'),
        executor: limited,
      ),
      limits: const TransportLimits(responseBytes: 4),
    ).execute(getAccount(const GetAccountVariables(id: 'acct-1')));
  });

  final _FixtureExecutor decompressed = _FixtureExecutor();
  decompressed.responses.add(
    (_) => AdapterResponse(
      status: 200,
      headers: const <String, String>{
        'content-type': 'application/vnd.naatre.response+json;version=1',
      },
      body: Stream<List<int>>.value(Uint8List(9)),
      close: () async {},
      wireBytes: 4,
    ),
  );
  await _expectTransportFailure('CLIENT_RESPONSE_LIMIT', () async {
    await NaatreClient(
      transport: HttpTransportAdapter.withExecutor(
        endpoint: Uri.parse('https://api.example/v1/execute'),
        executor: decompressed,
      ),
      limits: const TransportLimits(responseBytes: 4, decompressedBytes: 8),
    ).execute(getAccount(const GetAccountVariables(id: 'acct-1')));
  });
}

Future<void> _sseAndWebSocket() async {
  final _FixtureExecutor sse = _FixtureExecutor();
  int sseCloses = 0;
  sse.responses.add(
    (_) => AdapterResponse(
      status: 200,
      headers: const <String, String>{'content-type': 'text/event-stream'},
      body: Stream<List<int>>.fromIterable(<List<int>>[
        utf8.encode(
          'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n',
        ),
        utf8.encode(
          'event: naatre.complete\ndata: {"type":"complete","stream":"s","sequence":2}\n\n',
        ),
      ]),
      close: () async => sseCloses += 1,
    ),
  );
  final List<StreamFrame> sseFrames = await NaatreClient(
    sseTransport: PostSseTransportAdapter.withExecutor(
      endpoint: Uri.parse('https://api.example/v1/execute'),
      executor: sse,
    ),
  ).stream(getAccount(const GetAccountVariables(id: 'acct-1'))).toList();
  check(sseFrames.length == 2, 'POST-SSE adapter lost frames');
  check(sseCloses == 1, 'POST-SSE adapter did not release response');
  check(sse.cancellations == 1, 'POST-SSE adapter did not cancel upstream I/O');

  final _FixtureExecutor socket = _FixtureExecutor();
  int socketCloses = 0;
  socket.responses.add(
    (_) => AdapterResponse(
      status: 101,
      headers: const <String, String>{},
      body: Stream<List<int>>.fromIterable(<List<int>>[
        utf8.encode('{"type":"open","stream":"s","sequence":1}'),
        utf8.encode('{"type":"complete","stream":"s","sequence":2}'),
      ]),
      close: () async => socketCloses += 1,
    ),
  );
  final List<StreamFrame> socketFrames =
      await NaatreClient(
            webSocketTransport: WebSocketTransportAdapter.withExecutor(
              endpoint: Uri.parse('wss://api.example/v1/stream'),
              executor: socket,
            ),
          )
          .stream(
            getAccount(const GetAccountVariables(id: 'acct-1')),
            protocol: StreamProtocol.webSocket,
          )
          .toList();
  check(socketFrames.length == 2, 'WebSocket adapter lost JSON frames');
  check(socketCloses == 1, 'WebSocket adapter did not release socket');
  check(
    socket.cancellations == 1,
    'WebSocket adapter did not cancel upstream I/O',
  );

  final _FixtureExecutor oversized = _FixtureExecutor();
  oversized.responses.add(
    (_) => AdapterResponse(
      status: 101,
      headers: const <String, String>{},
      body: Stream<List<int>>.value(Uint8List(65)),
      close: () async {},
    ),
  );
  await _expectTransportFailure('CLIENT_STREAM_LIMIT', () async {
    await NaatreClient(
          webSocketTransport: WebSocketTransportAdapter.withExecutor(
            endpoint: Uri.parse('wss://api.example/v1/stream'),
            executor: oversized,
          ),
          limits: const TransportLimits(frameBytes: 64),
        )
        .stream(
          getAccount(const GetAccountVariables(id: 'acct-1')),
          protocol: StreamProtocol.webSocket,
        )
        .drain<void>();
  });
}

Future<void> _streamCancellationAndTotalLimit() async {
  final _BlockingExecutor handshaking = _BlockingExecutor();
  final CancellationController handshakeCancellation = CancellationController();
  final Future<List<StreamFrame>> unopened =
      NaatreClient(
            sseTransport: PostSseTransportAdapter.withExecutor(
              endpoint: Uri.parse('https://api.example/v1/execute'),
              executor: handshaking,
            ),
          )
          .stream(
            getAccount(const GetAccountVariables(id: 'acct-1')),
            cancellation: handshakeCancellation.token,
          )
          .toList();
  await Future<void>.delayed(Duration.zero);
  handshakeCancellation.cancel();
  await expectAsyncClientError('CLIENT_CANCELLED', () async => unopened);
  check(
    handshaking.cancellations == 1,
    'stream handshake cancellation did not abort upstream I/O',
  );

  final StreamController<List<int>> source = StreamController<List<int>>();
  final _FixtureExecutor cancellable = _FixtureExecutor();
  int closes = 0;
  cancellable.responses.add(
    (_) => AdapterResponse(
      status: 200,
      headers: const <String, String>{'content-type': 'text/event-stream'},
      body: source.stream,
      close: () async => closes += 1,
    ),
  );
  final CancellationController cancellation = CancellationController();
  final Future<List<StreamFrame>> pending =
      NaatreClient(
            sseTransport: PostSseTransportAdapter.withExecutor(
              endpoint: Uri.parse('https://api.example/v1/execute'),
              executor: cancellable,
            ),
          )
          .stream(
            getAccount(const GetAccountVariables(id: 'acct-1')),
            cancellation: cancellation.token,
          )
          .toList();
  await Future<void>.delayed(Duration.zero);
  source.add(
    utf8.encode(
      'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n',
    ),
  );
  await Future<void>.delayed(Duration.zero);
  cancellation.cancel();
  await expectAsyncClientError('CLIENT_CANCELLED', () async => pending);
  check(closes == 1, 'stream cancellation did not release the response');
  check(
    cancellable.cancellations == 1,
    'stream cancellation did not abort upstream I/O',
  );
  if (!source.isClosed) await source.close();

  final _FixtureExecutor totalLimited = _FixtureExecutor();
  totalLimited.responses.add(
    (_) => AdapterResponse(
      status: 200,
      headers: const <String, String>{'content-type': 'text/event-stream'},
      body: Stream<List<int>>.fromIterable(<List<int>>[
        Uint8List(40),
        Uint8List(40),
      ]),
      close: () async {},
    ),
  );
  await _expectTransportFailure('CLIENT_RESPONSE_LIMIT', () async {
    await NaatreClient(
      sseTransport: PostSseTransportAdapter.withExecutor(
        endpoint: Uri.parse('https://api.example/v1/execute'),
        executor: totalLimited,
      ),
      limits: const TransportLimits(decompressedBytes: 64, frameBytes: 48),
    ).stream(getAccount(const GetAccountVariables(id: 'acct-1'))).drain<void>();
  });
}

void _runtimeAndNumericBoundaries() {
  check(
    transportAdapterRuntime == 'dart-io' ||
        transportAdapterRuntime == 'browser',
    'conditional adapter import selected an unsupported runtime',
  );
  expectClientError(
    'CLIENT_VALUE_PRECISION',
    () => canonicalJson(int.parse('9007199254740992')),
  );
  expectClientError(
    'CLIENT_CONFIG_INVALID',
    () => WebSocketTransportAdapter(endpoint: Uri.parse('ws://api.example')),
  );
}

Future<void> _expectTransportFailure(
  String code,
  Future<void> Function() callback,
) async {
  try {
    await callback();
  } on TransportFailure catch (error) {
    check(error.code == code, 'expected $code, received ${error.code}');
    check(
      !error.toString().contains('secret'),
      'transport failure exposed protected data',
    );
    return;
  }
  throw StateError('expected $code');
}
