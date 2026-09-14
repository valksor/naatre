import 'package:naatre/naatre.dart';

void check(bool condition, String message) {
  if (!condition) throw StateError(message);
}

void expectClientError(String code, Object? Function() callback) {
  try {
    callback();
  } on NaatreClientException catch (error) {
    check(error.code == code, 'expected $code, received ${error.code}');
    return;
  }
  throw StateError('expected $code');
}

Future<void> expectAsyncClientError(
  String code,
  Future<void> Function() callback,
) async {
  try {
    await callback();
  } on NaatreClientException catch (error) {
    check(error.code == code, 'expected $code, received ${error.code}');
    return;
  }
  throw StateError('expected $code');
}
