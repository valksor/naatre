import 'worker_web.dart'
    if (dart.library.io) 'worker_vm.dart'
    as implementation;

Future<T> runInBackground<T>(T Function() task) => implementation.run<T>(task);
