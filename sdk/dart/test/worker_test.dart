import 'package:naatre/naatre.dart';

import 'support.dart';

Future<void> main() async {
  final String result = await runInBackground<String>(_backgroundValue);
  check(
    result == '{"portable":true}',
    'background execution changed canonical bytes',
  );
}

String _backgroundValue() => canonicalJson(<String, Object?>{'portable': true});
