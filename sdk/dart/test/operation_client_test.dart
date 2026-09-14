import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:naatre/generated.dart';
import 'package:naatre/naatre.dart';

import 'support.dart';

final class _RecordingTransport implements UnaryTransport {
  _RecordingTransport(this.payload);

  final List<int> payload;
  final List<Uint8List> requests = <Uint8List>[];
  int calls = 0;

  @override
  UnaryExchange send(Uint8List request, TransportContext context) {
    calls += 1;
    requests.add(request);
    return UnaryExchange(
      Future<TransportResponse>.value(TransportResponse(body: payload)),
      () async {},
    );
  }
}

final class _CancellableTransport implements UnaryTransport {
  final Completer<TransportResponse> response = Completer<TransportResponse>();
  final Completer<void> released = Completer<void>();

  @override
  UnaryExchange send(Uint8List request, TransportContext context) {
    return UnaryExchange(response.future, () async {
      if (!released.isCompleted) released.complete();
    });
  }
}

final class _FailingTransport implements UnaryTransport {
  int calls = 0;

  @override
  UnaryExchange send(Uint8List request, TransportContext context) {
    calls += 1;
    return UnaryExchange(
      Future<TransportResponse>.error(
        const TransportFailure('NETWORK', retryable: true),
      ),
      () async {},
    );
  }
}

Future<void> main() async {
  final Operation<GetAccountVariables, GetAccountResult> missing = getAccount(
    const GetAccountVariables(id: 'acct-1'),
  );
  final Operation<GetAccountVariables, GetAccountResult> explicitNull =
      getAccount(
        const GetAccountVariables(
          id: 'acct-1',
          nickname: InputValue<String?>.present(null),
        ),
      );
  check(
    !utf8.decode(missing.canonicalRequest()).contains('nickname'),
    'missing was serialized',
  );
  check(
    utf8.decode(explicitNull.canonicalRequest()).contains('"nickname":null'),
    'explicit null was omitted',
  );

  final List<int> response = utf8.encode(
    '{"complete":false,"data":{"profile":{"display":"Ada","nickname":null}},'
    '"errors":[{"code":"PARTIAL","future":"retained","path":["later"]}]}',
  );
  final _RecordingTransport transport = _RecordingTransport(response);
  final OperationResult<GetAccountResult> result = await NaatreClient(
    transport: transport,
  ).execute(explicitNull);
  check(result.complete == false, 'partial completion was lost');
  check(
    result.data?.profile.state == PresenceState.present,
    'profile presence drifted',
  );
  check(
    result.data?.profile.value?.nickname.state == PresenceState.nullValue,
    'selected null drifted',
  );
  check(
    result.data?.later.state == PresenceState.pending,
    'pending state drifted',
  );
  check(
    result.errors.single.raw['future'] == 'retained',
    'unknown error fields were lost',
  );
  check(
    utf8.decode(transport.requests.single) ==
        '{"operation":"GetAccount","persisted":{"algorithm":"sha-256","canonicalVersion":"c14n-1","digest":"cc863005080edcb85ec0345a50593dc58111896bbbe7ff86475ef2412fc5cb90"},"variables":{"id":"acct-1","nickname":null},"version":"1"}',
    'canonical persisted request drifted',
  );

  final _CancellableTransport cancellable = _CancellableTransport();
  final CancellationController controller = CancellationController();
  final Future<OperationResult<GetAccountResult>> pending = NaatreClient(
    transport: cancellable,
  ).execute(missing, cancellation: controller.token);
  controller.cancel();
  await expectAsyncClientError('CLIENT_CANCELLED', () async {
    await pending;
  });
  await cancellable.released.future;

  final _CancellableTransport expiring = _CancellableTransport();
  await expectAsyncClientError('CLIENT_DEADLINE_EXCEEDED', () async {
    await NaatreClient(transport: expiring).execute(
      missing,
      deadline: DateTime.now().toUtc().add(const Duration(milliseconds: 1)),
    );
  });
  await expiring.released.future;

  await expectAsyncClientError('CLIENT_RETRY_INVALID', () async {
    await NaatreClient(
      transport: transport,
      retryPolicy: RetryPolicy(maximumAttempts: int.parse('0')),
    ).execute(missing);
  });

  final Selected<String> failed = Selected<String>.failed(result.errors);
  final Selected<String> skipped = Selected<String>.skipped('dependency');
  check(
    failed.state == PresenceState.failed && failed.errors.length == 1,
    'failed selected state drifted',
  );
  check(
    skipped.state == PresenceState.skipped && skipped.reason == 'dependency',
    'skipped selected state drifted',
  );

  final _FailingTransport failing = _FailingTransport();
  final Operation<GetAccountVariables, GetAccountResult> mutation = Operation(
    name: 'WriteAccount',
    kind: 'mutation',
    persisted: missing.persisted,
    variables: missing.variables,
    wireVariables: missing.wireVariables,
    decodeData: (Object? value) => result.data!,
  );
  try {
    await NaatreClient(
      transport: failing,
      retryPolicy: const RetryPolicy(maximumAttempts: 3),
    ).execute(mutation);
  } on TransportFailure {
    // Expected.
  }
  check(failing.calls == 1, 'non-idempotent write was replayed');

  final Map<String, String> redirected = const RedirectPolicy()
      .headersForRedirect(
        from: Uri.parse('https://first.example/a'),
        to: Uri.parse('https://second.example/b'),
        headers: const <String, String>{
          'Authorization': 'secret',
          'X-Naatre-Tenant': 'tenant',
          'Accept': 'application/json',
        },
        redirectCount: 0,
      );
  check(
    !redirected.containsKey('Authorization'),
    'redirect retained credentials',
  );
  check(
    redirected['Accept'] == 'application/json',
    'redirect removed safe header',
  );
}
