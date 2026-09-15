<?php

declare(strict_types=1);

namespace Naatre\Sdk\Transport;

use Naatre\Sdk\Exception\ClientException;
use Psr\Http\Message\ResponseInterface;
use Throwable;

/** @internal */
final class BoundedResponseBody
{
    public static function read(ResponseInterface $response, int $maximumBytes): string
    {
        $body = null;
        $failed = false;
        try {
            $body = $response->getBody();
            $contentType = strtolower(trim(explode(';', $response->getHeaderLine('Content-Type'), 2)[0]));
            if ($contentType !== 'application/vnd.naatre.response+json') {
                throw new ClientException('CLIENT_MEDIA_TYPE_INVALID');
            }
            $contents = '';
            while (true) {
                $chunk = $body->read(min(8192, $maximumBytes - strlen($contents) + 1));
                $contents .= $chunk;
                if (strlen($contents) > $maximumBytes) {
                    throw new ClientException('CLIENT_RESPONSE_TOO_LARGE');
                }
                if ($body->eof()) {
                    break;
                }
                if ($chunk === '') {
                    throw new ClientException('CLIENT_RESPONSE_STALLED');
                }
            }
            return $contents;
        } catch (Throwable $error) {
            $failed = true;
            if ($error instanceof ClientException) {
                throw $error;
            }
            throw new ClientException('CLIENT_TRANSPORT_ERROR');
        } finally {
            try {
                $body?->close();
            } catch (Throwable) {
                if (!$failed) {
                    throw new ClientException('CLIENT_TRANSPORT_ERROR');
                }
            }
        }
    }
}
