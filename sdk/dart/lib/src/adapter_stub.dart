import 'adapter_engine.dart';
import 'error.dart';

AdapterExecutor createPlatformAdapterExecutor() =>
    const _UnsupportedAdapterExecutor();

const String platformAdapterRuntime = 'unsupported';

final class _UnsupportedAdapterExecutor implements AdapterExecutor {
  const _UnsupportedAdapterExecutor();

  @override
  String get runtime => platformAdapterRuntime;

  @override
  AdapterExchange start(AdapterRequest request) {
    throw const NaatreClientException('CLIENT_CAPABILITY_UNSUPPORTED:runtime');
  }
}
