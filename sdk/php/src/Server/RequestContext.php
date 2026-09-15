<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

final class RequestContext
{
    private ?Principal $principal;
    private ?Transaction $transaction = null;
    private bool $closed = false;

    public function __construct(
        public readonly string $requestId,
        public readonly string $invocationId,
        Principal $principal,
        private readonly LoaderCache $loaders = new LoaderCache(),
    ) {
        $this->principal = $principal;
    }

    public function principal(): Principal
    {
        if ($this->closed || $this->principal === null) {
            throw new ServerException('REMOTE_CONTEXT_CLOSED');
        }
        return $this->principal;
    }

    public function loaders(): LoaderCache
    {
        if ($this->closed) {
            throw new ServerException('REMOTE_CONTEXT_CLOSED');
        }
        return $this->loaders;
    }

    public function attachTransaction(Transaction $transaction): void
    {
        if ($this->closed || $this->transaction !== null) {
            throw new ServerException('REMOTE_TRANSACTION_INVALID');
        }
        $this->transaction = $transaction;
    }

    public function close(bool $success): void
    {
        if ($this->closed) {
            return;
        }
        $this->closed = true;
        try {
            if ($this->transaction !== null) {
                $success ? $this->transaction->commit() : $this->transaction->rollback();
            }
        } finally {
            $this->transaction = null;
            $this->loaders->clear();
            $this->principal = null;
        }
    }
}
