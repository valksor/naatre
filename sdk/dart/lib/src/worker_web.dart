Future<T> run<T>(T Function() task) => Future<T>.sync(task);
