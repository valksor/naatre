import 'dart:isolate';

Future<T> run<T>(T Function() task) => Isolate.run<T>(task);
