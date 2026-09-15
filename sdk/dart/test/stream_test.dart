import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:naatre/generated.dart';
import 'package:naatre/naatre.dart';

import 'support.dart';

final class _FixtureSseTransport implements SseTransport {
  _FixtureSseTransport(this.controller);

  final StreamController<List<int>> controller;
  int closes = 0;

  @override
  Future<StreamConnection> open(
    Uint8List request,
    TransportContext context,
  ) async {
    return StreamConnection(controller.stream, () async {
      closes += 1;
      if (!controller.isClosed) await controller.close();
    });
  }
}

Future<void> main() async {
  await _terminalAndTruncation();
  await _orderingAndChunkBoundaries();
  await _cancellationAndBackpressure();
}

Future<void> _terminalAndTruncation() async {
  bool sourceCancelled = false;
  int closes = 0;
  final StreamController<List<int>> source = StreamController<List<int>>(
    onCancel: () => sourceCancelled = true,
  );
  final Future<List<StreamFrame>> completed = decodeSse(
    source.stream,
    maximumFrameBytes: 100,
    close: () async => closes += 1,
  ).toList();
  source.add(
    utf8.encode(
      'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n'
      'event: naatre.complete\ndata: {"type":"complete","stream":"s","sequence":2}\n\n',
    ),
  );
  final List<StreamFrame> frames = await completed;
  check(
    frames.length == 2 && frames.last.terminal,
    'terminal frame was not accepted',
  );
  check(sourceCancelled, 'terminal frame did not release the source');
  check(
    closes == 1,
    'terminal frame did not close the connection exactly once',
  );
  await source.close();
  await expectAsyncClientError('CLIENT_STREAM_TRUNCATED', () async {
    await decodeSse(
      Stream<List<int>>.value(
        utf8.encode(
          'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n',
        ),
      ),
    ).drain<void>();
  });
}

Future<void> _orderingAndChunkBoundaries() async {
  await expectAsyncClientError(
    'CLIENT_CAPABILITY_UNSUPPORTED:websocket',
    () async {
      await const NaatreClient()
          .stream(
            getAccount(const GetAccountVariables(id: 'acct-1')),
            protocol: StreamProtocol.webSocket,
          )
          .drain<void>();
    },
  );
  for (final String invalid in <String>[
    'event: naatre.data\ndata: {"type":"data","stream":"s","sequence":1,"data":{}}\n\n',
    <String>[
      'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n',
      'event: naatre.data\ndata: {"type":"data","stream":"s","sequence":3,"data":{}}\n\n',
    ].join(),
    <String>[
      'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n',
      'event: naatre.complete\ndata: {"type":"complete","stream":"other","sequence":2}\n\n',
    ].join(),
  ]) {
    await expectAsyncClientError('CLIENT_STREAM_INVALID', () async {
      await decodeSse(
        Stream<List<int>>.value(utf8.encode(invalid)),
      ).drain<void>();
    });
  }
}

Future<void> _cancellationAndBackpressure() async {
  bool paused = false;
  bool resumed = false;
  bool upstreamCancelled = false;
  final Completer<void> upstreamReleased = Completer<void>();
  late final StreamController<List<int>> source;
  source = StreamController<List<int>>(
    onPause: () => paused = true,
    onResume: () => resumed = true,
    onCancel: () {
      upstreamCancelled = true;
      if (!upstreamReleased.isCompleted) upstreamReleased.complete();
    },
  );
  final _FixtureSseTransport transport = _FixtureSseTransport(source);
  final CancellationController cancellation = CancellationController();
  final Completer<void> firstReceived = Completer<void>();
  final Completer<void> secondReceived = Completer<void>();
  int received = 0;
  late final StreamSubscription<StreamFrame> subscription;
  subscription = NaatreClient(sseTransport: transport)
      .stream(
        getAccount(const GetAccountVariables(id: 'acct-1')),
        cancellation: cancellation.token,
      )
      .listen((StreamFrame _) {
        received += 1;
        subscription.pause();
        if (received == 1) {
          firstReceived.complete();
        } else if (received == 2) {
          secondReceived.complete();
        }
      });
  final Future<void> completed = subscription.asFuture<void>();
  source.add(
    utf8.encode(
      'event: naatre.open\ndata: {"type":"open","stream":"s","sequence":1}\n\n',
    ),
  );
  await firstReceived.future;
  check(paused, 'stream pause did not reach the upstream source');
  subscription.resume();
  await Future<void>.delayed(Duration.zero);
  check(resumed, 'stream resume did not reach the upstream source');
  source.add(
    utf8.encode(
      'event: naatre.data\ndata: {"type":"data","stream":"s","sequence":2,"data":{}}\n\n',
    ),
  );
  await secondReceived.future;
  cancellation.cancel();
  await upstreamReleased.future;
  await Future<void>.delayed(Duration.zero);
  check(transport.closes == 1, 'paused cancellation did not close transport');
  subscription.resume();
  try {
    await completed;
  } on NaatreClientException catch (error) {
    check(error.code == 'CLIENT_CANCELLED', 'wrong cancellation error');
  } finally {
    await subscription.cancel();
    if (!source.isClosed) await source.close();
  }
  check(transport.closes == 1, 'stream transport was not closed exactly once');
  check(upstreamCancelled, 'stream subscription was not released');
}
