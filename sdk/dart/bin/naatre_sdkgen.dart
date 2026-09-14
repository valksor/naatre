import 'dart:io';

import 'package:naatre/naatre.dart';

Future<void> main(List<String> arguments) async {
  if (arguments.length != 3) {
    stderr.writeln('usage: naatre_sdkgen MODEL REFERENCE OUTPUT_ROOT');
    exitCode = 2;
    return;
  }
  final File model = File(arguments[0]);
  final File reference = File(arguments[1]);
  final Directory output = Directory(arguments[2]).absolute;
  await output.create(recursive: true);
  final DartArtifacts artifacts = generateDartSdk(
    await model.readAsBytes(),
    await reference.readAsBytes(),
  );
  await _replace(File('${output.path}/operations.dart'), artifacts.source);
  await _replace(File('${output.path}/operations.json'), artifacts.manifest);
}

Future<void> _replace(File target, List<int> bytes) async {
  final File temporary = File('${target.path}.tmp-${pid.toString()}');
  try {
    await temporary.writeAsBytes(bytes, flush: true);
    await temporary.rename(target.path);
  } finally {
    if (await temporary.exists()) await temporary.delete();
  }
}
