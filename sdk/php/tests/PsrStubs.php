<?php

declare(strict_types=1);

namespace Psr\Http\Message {
    interface StreamInterface
    {
        public function __toString(): string;
        public function close(): void;
        /** @phpstan-impure */
        public function eof(): bool;
        public function isReadable(): bool;
        public function read(int $length): string;
    }

    interface RequestInterface
    {
        public function withHeader(string $name, string $value): self;
        public function getHeaderLine(string $name): string;
        public function withBody(StreamInterface $body): self;
        public function getBody(): StreamInterface;
    }

    interface ResponseInterface
    {
        public function getStatusCode(): int;
        public function getHeaderLine(string $name): string;
        public function getBody(): StreamInterface;
    }

    interface RequestFactoryInterface
    {
        public function createRequest(string $method, mixed $uri): RequestInterface;
    }

    interface StreamFactoryInterface
    {
        public function createStream(string $content = ''): StreamInterface;
    }
}

namespace Psr\Http\Client {
    use Psr\Http\Message\RequestInterface;
    use Psr\Http\Message\ResponseInterface;

    interface ClientExceptionInterface extends \Throwable
    {
    }

    interface ClientInterface
    {
        public function sendRequest(RequestInterface $request): ResponseInterface;
    }
}

namespace Naatre\Sdk\Tests {
    use Psr\Http\Client\ClientExceptionInterface;
    use Psr\Http\Client\ClientInterface;
    use Psr\Http\Message\RequestFactoryInterface;
    use Psr\Http\Message\RequestInterface;
    use Psr\Http\Message\ResponseInterface;
    use Psr\Http\Message\StreamFactoryInterface;
    use Psr\Http\Message\StreamInterface;

    final class TestStream implements StreamInterface
    {
        private int $offset = 0;
        private bool $closed = false;

        public function __construct(
            private readonly string $contents = '',
            private readonly bool $stall = false,
            private readonly bool $failClose = false,
        )
        {
        }

        public function __toString(): string
        {
            return $this->contents;
        }

        public function close(): void
        {
            if ($this->failClose) {
                throw new \RuntimeException('Bearer protected-close-detail');
            }
            $this->closed = true;
        }

        public function eof(): bool
        {
            return $this->closed || (!$this->stall && $this->offset >= strlen($this->contents));
        }

        public function isReadable(): bool
        {
            return !$this->closed;
        }

        public function read(int $length): string
        {
            if ($this->closed) {
                throw new \RuntimeException('stream closed');
            }
            $chunk = $this->stall ? '' : substr($this->contents, $this->offset, $length);
            $this->offset += strlen($chunk);
            return $chunk;
        }
    }

    final class TestRequest implements RequestInterface
    {
        /** @param array<string, string> $headers */
        public function __construct(private array $headers = [], private ?StreamInterface $body = null)
        {
        }

        public function withHeader(string $name, string $value): self
        {
            $copy = clone $this;
            $copy->headers[strtolower($name)] = $value;
            return $copy;
        }

        public function getHeaderLine(string $name): string
        {
            return $this->headers[strtolower($name)] ?? '';
        }

        public function withBody(StreamInterface $body): self
        {
            $copy = clone $this;
            $copy->body = $body;
            return $copy;
        }

        public function getBody(): StreamInterface
        {
            return $this->body ?? new TestStream();
        }
    }

    final readonly class TestResponse implements ResponseInterface
    {
        public StreamInterface $body;

        /** @param array<string, string> $headers */
        public function __construct(private array $headers, string|StreamInterface $body, private int $statusCode = 200)
        {
            $this->body = is_string($body) ? new TestStream($body) : $body;
        }

        public function getStatusCode(): int
        {
            return $this->statusCode;
        }

        public function getHeaderLine(string $name): string
        {
            return $this->headers[strtolower($name)] ?? $this->headers[$name] ?? '';
        }

        public function getBody(): StreamInterface
        {
            return $this->body;
        }
    }

    final class TestFactory implements ClientInterface, RequestFactoryInterface, StreamFactoryInterface
    {
        public ?RequestInterface $request = null;
        public int $attempts = 0;

        /** @var list<RequestInterface> */
        public array $requests = [];

        /** @param ResponseInterface|list<ResponseInterface>|null $responses */
        public function __construct(
            private array|ResponseInterface|null $responses = null,
            private readonly int $failuresBeforeSuccess = 0,
            private readonly string $failureMessage = 'temporary transport failure',
        )
        {
        }

        public function createRequest(string $method, mixed $uri): RequestInterface
        {
            return new TestRequest();
        }

        public function createStream(string $content = ''): StreamInterface
        {
            return new TestStream($content);
        }

        public function sendRequest(RequestInterface $request): ResponseInterface
        {
            $this->request = $request;
            $this->requests[] = $request;
            ++$this->attempts;
            if ($this->attempts <= $this->failuresBeforeSuccess) {
                throw new TestTransportException($this->failureMessage);
            }
            if ($this->responses === null) {
                throw new \RuntimeException('test HTTP client is not configured');
            }
            if ($this->responses instanceof ResponseInterface) {
                return $this->responses;
            }
            $response = array_shift($this->responses);
            if (!$response instanceof ResponseInterface) {
                throw new \RuntimeException('fixture response exhausted');
            }
            return $response;
        }
    }

    final class TestTransportException extends \RuntimeException implements ClientExceptionInterface
    {
    }
}
