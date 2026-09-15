<?php

declare(strict_types=1);

namespace Naatre\Sdk\Server;

use Naatre\Sdk\Value\ObjectValue;
use Throwable;

final readonly class Dispatcher
{
    public function __construct(
        private HandlerRegistry $handlers,
        private RuntimeProfile $profile,
        private RequestContextFactory $contexts,
        private ?TransactionProvider $transactions = null,
    ) {
    }

    public function dispatch(Invocation $invocation): WorkerResult
    {
        $handler = $this->handlers->get($invocation->handlerId);
        if (!$this->profile->supports($handler->requiredCapabilities)) {
            throw new ServerException('REMOTE_CAPABILITY_MISMATCH');
        }
        if (!$invocation->input instanceof ObjectValue) {
            throw new ServerException('REMOTE_INVOCATION_INVALID');
        }
        $context = $this->contexts->create($invocation);
        $success = false;
        try {
            if ($handler->usesTransaction) {
                if ($this->transactions === null) {
                    throw new ServerException('REMOTE_CAPABILITY_MISMATCH');
                }
                $context->attachTransaction($this->transactions->begin($context));
            }
            $input = ($handler->decodeInput)($invocation->input);
            $output = ($handler->invoke)($input, $context);
            $wire = ($handler->encodeOutput)($output);
            ($handler->validateOutput)($wire);
            $success = true;
            return new WorkerResult($invocation->invocationId, $invocation->attemptId, $invocation->schemaRevision, $wire);
        } catch (ApplicationException $error) {
            return WorkerResult::applicationError($invocation, $error);
        } catch (ServerException $error) {
            throw $error;
        } catch (Throwable) {
            throw new ServerException('REMOTE_WORKER_MALFORMED');
        } finally {
            $context->close($success);
        }
    }
}
