#!/usr/bin/env python3

import json
import os
import grp
import pwd
import socket
import struct
import subprocess
import sys
import tempfile
import time
import urllib.parse


FCGI_BEGIN_REQUEST = 1
FCGI_END_REQUEST = 3
FCGI_PARAMS = 4
FCGI_STDIN = 5
FCGI_STDOUT = 6
FCGI_STDERR = 7


def record(kind: int, content: bytes, request_id: int = 1) -> bytes:
    padding = (-len(content)) % 8
    return struct.pack('!BBHHBB', 1, kind, request_id, len(content), padding, 0) + content + (b'\0' * padding)


def encoded_length(length: int) -> bytes:
    if length < 128:
        return bytes([length])
    return struct.pack('!I', length | 0x80000000)


def parameter(name: str, value: str) -> bytes:
    name_bytes = name.encode()
    value_bytes = value.encode()
    return encoded_length(len(name_bytes)) + encoded_length(len(value_bytes)) + name_bytes + value_bytes


def receive_exact(connection: socket.socket, length: int) -> bytes:
    chunks = []
    remaining = length
    while remaining:
        chunk = connection.recv(remaining)
        if not chunk:
            raise RuntimeError('unexpected FastCGI end of stream')
        chunks.append(chunk)
        remaining -= len(chunk)
    return b''.join(chunks)


def request(port: int, script: str, query: dict[str, str]) -> dict[str, object]:
    params = {
        'GATEWAY_INTERFACE': 'CGI/1.1',
        'REQUEST_METHOD': 'GET',
        'SCRIPT_FILENAME': script,
        'SCRIPT_NAME': '/fpm-isolation.php',
        'QUERY_STRING': urllib.parse.urlencode(query),
        'SERVER_PROTOCOL': 'HTTP/1.1',
        'SERVER_NAME': '127.0.0.1',
        'SERVER_PORT': str(port),
        'REMOTE_ADDR': '127.0.0.1',
        'REMOTE_PORT': '1',
    }
    payload = b''.join(parameter(name, value) for name, value in params.items())
    begin = struct.pack('!HB5x', 1, 0)
    with socket.create_connection(('127.0.0.1', port), timeout=5) as connection:
        connection.sendall(record(FCGI_BEGIN_REQUEST, begin))
        connection.sendall(record(FCGI_PARAMS, payload))
        connection.sendall(record(FCGI_PARAMS, b''))
        connection.sendall(record(FCGI_STDIN, b''))
        stdout = bytearray()
        stderr = bytearray()
        while True:
            header = receive_exact(connection, 8)
            _, kind, _, content_length, padding_length, _ = struct.unpack('!BBHHBB', header)
            content = receive_exact(connection, content_length)
            if padding_length:
                receive_exact(connection, padding_length)
            if kind == FCGI_STDOUT:
                stdout.extend(content)
            elif kind == FCGI_STDERR:
                stderr.extend(content)
            elif kind == FCGI_END_REQUEST:
                break
    if stderr:
        raise RuntimeError(stderr.decode(errors='replace'))
    parts = bytes(stdout).split(b'\r\n\r\n', 1)
    if len(parts) != 2:
        raise RuntimeError(bytes(stdout).decode(errors='replace'))
    return json.loads(parts[1])


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit('usage: isolation.py PHP_FPM SCRIPT')
    php_fpm = os.path.abspath(sys.argv[1])
    script = os.path.abspath(sys.argv[2])
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        port = probe.getsockname()[1]
    with tempfile.TemporaryDirectory(prefix='naatre-fpm-') as directory:
        config = os.path.join(directory, 'php-fpm.conf')
        log = os.path.join(directory, 'php-fpm.log')
        user = pwd.getpwuid(os.getuid()).pw_name
        group = grp.getgrgid(os.getgid()).gr_name
        with open(config, 'w', encoding='utf-8') as stream:
            stream.write(f'''[global]\nerror_log = {log}\ndaemonize = no\n[www]\nuser = {user}\ngroup = {group}\nlisten = 127.0.0.1:{port}\npm = static\npm.max_children = 1\ncatch_workers_output = yes\nclear_env = no\n''')
        arguments = [php_fpm, '--nodaemonize', '--fpm-config', config]
        if os.getuid() == 0:
            arguments.append('--allow-to-run-as-root')
        process = subprocess.Popen(arguments)
        try:
            deadline = time.monotonic() + 10
            while True:
                try:
                    first = request(port, script, {'tenant': 'alpha'})
                    break
                except (ConnectionRefusedError, OSError):
                    if process.poll() is not None or time.monotonic() >= deadline:
                        raise
                    time.sleep(0.05)
            failed = request(port, script, {'tenant': 'alpha', 'fail': '1'})
            changed = request(port, script, {'tenant': 'beta', 'previous': 'alpha'})
            if len({first['pid'], failed['pid'], changed['pid']}) != 1:
                raise RuntimeError('requests did not reuse one FPM worker')
            if not first['containsCurrent'] or failed['failureCode'] != 'CLIENT_JSON_INVALID':
                raise RuntimeError('success/failure lifecycle did not produce declared results')
            if not changed['containsCurrent'] or changed['containsPrevious']:
                raise RuntimeError('tenant state leaked between FPM requests')
            print(json.dumps({'profile': 'sdk.php.native.fpm-isolation-1', 'status': 'passed', 'requests': 3}))
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)


if __name__ == '__main__':
    main()
