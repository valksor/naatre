import 'dart:typed_data';

const List<int> _roundConstants = <int>[
  0x428a2f98,
  0x71374491,
  0xb5c0fbcf,
  0xe9b5dba5,
  0x3956c25b,
  0x59f111f1,
  0x923f82a4,
  0xab1c5ed5,
  0xd807aa98,
  0x12835b01,
  0x243185be,
  0x550c7dc3,
  0x72be5d74,
  0x80deb1fe,
  0x9bdc06a7,
  0xc19bf174,
  0xe49b69c1,
  0xefbe4786,
  0x0fc19dc6,
  0x240ca1cc,
  0x2de92c6f,
  0x4a7484aa,
  0x5cb0a9dc,
  0x76f988da,
  0x983e5152,
  0xa831c66d,
  0xb00327c8,
  0xbf597fc7,
  0xc6e00bf3,
  0xd5a79147,
  0x06ca6351,
  0x14292967,
  0x27b70a85,
  0x2e1b2138,
  0x4d2c6dfc,
  0x53380d13,
  0x650a7354,
  0x766a0abb,
  0x81c2c92e,
  0x92722c85,
  0xa2bfe8a1,
  0xa81a664b,
  0xc24b8b70,
  0xc76c51a3,
  0xd192e819,
  0xd6990624,
  0xf40e3585,
  0x106aa070,
  0x19a4c116,
  0x1e376c08,
  0x2748774c,
  0x34b0bcb5,
  0x391c0cb3,
  0x4ed8aa4a,
  0x5b9cca4f,
  0x682e6ff3,
  0x748f82ee,
  0x78a5636f,
  0x84c87814,
  0x8cc70208,
  0x90befffa,
  0xa4506ceb,
  0xbef9a3f7,
  0xc67178f2,
];

String sha256Hex(List<int> input) {
  final Uint8List message = _padded(input);
  final List<int> state = <int>[
    0x6a09e667,
    0xbb67ae85,
    0x3c6ef372,
    0xa54ff53a,
    0x510e527f,
    0x9b05688c,
    0x1f83d9ab,
    0x5be0cd19,
  ];
  final List<int> words = List<int>.filled(64, 0);
  for (int offset = 0; offset < message.length; offset += 64) {
    for (int index = 0; index < 16; index += 1) {
      final int position = offset + index * 4;
      words[index] =
          message[position] << 24 |
          message[position + 1] << 16 |
          message[position + 2] << 8 |
          message[position + 3];
    }
    for (int index = 16; index < 64; index += 1) {
      final int first =
          _rotate(words[index - 15], 7) ^
          _rotate(words[index - 15], 18) ^
          (words[index - 15] >>> 3);
      final int second =
          _rotate(words[index - 2], 17) ^
          _rotate(words[index - 2], 19) ^
          (words[index - 2] >>> 10);
      words[index] = _word(
        words[index - 16] + first + words[index - 7] + second,
      );
    }
    int a = state[0];
    int b = state[1];
    int c = state[2];
    int d = state[3];
    int e = state[4];
    int f = state[5];
    int g = state[6];
    int h = state[7];
    for (int index = 0; index < 64; index += 1) {
      final int upper = _rotate(e, 6) ^ _rotate(e, 11) ^ _rotate(e, 25);
      final int choose = (e & f) ^ ((~e) & g);
      final int first = _word(
        h + upper + choose + _roundConstants[index] + words[index],
      );
      final int lower = _rotate(a, 2) ^ _rotate(a, 13) ^ _rotate(a, 22);
      final int majority = (a & b) ^ (a & c) ^ (b & c);
      final int second = _word(lower + majority);
      h = g;
      g = f;
      f = e;
      e = _word(d + first);
      d = c;
      c = b;
      b = a;
      a = _word(first + second);
    }
    state[0] = _word(state[0] + a);
    state[1] = _word(state[1] + b);
    state[2] = _word(state[2] + c);
    state[3] = _word(state[3] + d);
    state[4] = _word(state[4] + e);
    state[5] = _word(state[5] + f);
    state[6] = _word(state[6] + g);
    state[7] = _word(state[7] + h);
  }
  return state
      .map((int value) => value.toRadixString(16).padLeft(8, '0'))
      .join();
}

Uint8List _padded(List<int> input) {
  final int bitLength = input.length * 8;
  final int totalLength = ((input.length + 9 + 63) ~/ 64) * 64;
  final Uint8List result = Uint8List(totalLength)
    ..setRange(0, input.length, input);
  result[input.length] = 0x80;
  for (int index = 0; index < 8; index += 1) {
    result[totalLength - 1 - index] = (bitLength >>> (index * 8)) & 0xff;
  }
  return result;
}

int _rotate(int value, int count) =>
    _word((value >>> count) | (value << (32 - count)));

int _word(int value) => value & 0xffffffff;
