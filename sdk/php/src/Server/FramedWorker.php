<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Naatre\Sdk\Value\ObjectValue;
use Naatre\Sdk\Wire\Json;

final class FramedWorker
{
    /** @param resource $input @param resource $output */
    public function __construct(
        private readonly EnvelopeServer $server,
        private $input,
        private $output,
        private readonly int $maximumFrameBytes = 65_536,
        private readonly bool $ownsStreams = false,
    ) {
        if (!is_resource($input) || !is_resource($output) || $maximumFrameBytes < 1) {
            throw new ServerException('REMOTE_REGISTRATION_INVALID');
        }
    }

    public function run(): void
    {
        try {
            while (($header = $this->read(5, true)) !== null) {
                $parts = unpack('Cflags/Nlength', $header);
                if (!is_array($parts) || $parts['flags'] !== 0 || $parts['length'] < 1 || $parts['length'] > $this->maximumFrameBytes) {
                    throw new ServerException('REMOTE_WORKER_MALFORMED');
                }
                $payload = $this->read($parts['length']);
                if ($payload === null) {
                    throw new ServerException('REMOTE_WORKER_MALFORMED');
                }
                $decoded = Json::decode($payload, $this->maximumFrameBytes);
                if (!$decoded instanceof ObjectValue) {
                    throw new ServerException('REMOTE_WORKER_MALFORMED');
                }
                $this->write(Json::encode($this->server->handle($decoded)));
            }
        } finally {
            if ($this->ownsStreams) {
                fclose($this->input);
                fclose($this->output);
            }
        }
    }

    private function read(int $bytes, bool $allowEof = false): ?string
    {
        $value = '';
        while (strlen($value) < $bytes) {
            $chunk = fread($this->input, $bytes - strlen($value));
            if ($chunk === false || ($chunk === '' && !feof($this->input))) {
                throw new ServerException('REMOTE_WORKER_MALFORMED');
            }
            if ($chunk === '') {
                if ($allowEof && $value === '') {
                    return null;
                }
                throw new ServerException('REMOTE_WORKER_MALFORMED');
            }
            $value .= $chunk;
        }
        return $value;
    }

    private function write(string $payload): void
    {
        if ($payload === '' || strlen($payload) > $this->maximumFrameBytes) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        }
        $frame = pack('CN', 0, strlen($payload)) . $payload;
        while ($frame !== '') {
            $written = fwrite($this->output, $frame);
            if ($written === false || $written < 1) {
                throw new ServerException('REMOTE_WORKER_MALFORMED');
            }
            $frame = substr($frame, $written);
        }
        fflush($this->output);
    }
}
