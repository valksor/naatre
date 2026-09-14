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

        public function __construct(private readonly string $contents)
        {
        }

        public function __toString(): string
        {
            return $this->contents;
        }

        public function close(): void
        {
            $this->closed = true;
        }

        public function eof(): bool
        {
            return $this->closed || $this->offset >= strlen($this->contents);
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
            $chunk = substr($this->contents, $this->offset, $length);
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
        public TestStream $body;

        /** @param array<string, string> $headers */
        public function __construct(private array $headers, string $body)
        {
            $this->body = new TestStream($body);
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

    final class TestFactory implements RequestFactoryInterface, StreamFactoryInterface
    {
        public function createRequest(string $method, mixed $uri): RequestInterface
        {
            return new TestRequest();
        }

        public function createStream(string $content = ''): StreamInterface
        {
            return new TestStream($content);
        }
    }

    final class TestHttpClient implements ClientInterface
    {
        public ?RequestInterface $request = null;

        public function __construct(private readonly ResponseInterface $response)
        {
        }

        public function sendRequest(RequestInterface $request): ResponseInterface
        {
            $this->request = $request;
            return $this->response;
        }
    }
}
