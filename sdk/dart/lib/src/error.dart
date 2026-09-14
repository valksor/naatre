class NaatreClientException implements Exception {
  const NaatreClientException(this.code, [this.cause]);

  final String code;
  final Object? cause;

  @override
  String toString() => 'NaatreClientException: $code';
}

Never fail(String code, [Object? cause]) {
  throw NaatreClientException(code, cause);
}
