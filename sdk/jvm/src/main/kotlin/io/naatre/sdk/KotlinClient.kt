package io.naatre.sdk

import io.naatre.sdk.ClientCore.Client
import io.naatre.sdk.ClientCore.ClientException
import io.naatre.sdk.ClientCore.ErrorCode
import io.naatre.sdk.ClientCore.Operation
import io.naatre.sdk.ClientCore.OperationResult
import java.time.Duration
import java.util.concurrent.Flow as JdkFlow
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicReference
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.channels.onFailure
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

/** Coroutine and Flow views over the same cancellable Java transport call. */
class KotlinClient(private val delegate: Client) {
    suspend fun <V : Any, R : Any> execute(
        operation: Operation<V, R>,
        variables: V,
        deadline: Duration? = null,
    ): OperationResult<R> = suspendCancellableCoroutine { continuation ->
        val future = delegate.executeAsync(operation, variables, deadline)
        continuation.invokeOnCancellation { future.cancel(true) }
        future.whenComplete { result, failure ->
            if (failure == null) {
                if (continuation.isActive) continuation.resume(result)
            } else if (continuation.isActive) {
                continuation.resumeWithException(failure)
            }
        }
    }

    fun <V : Any, R : Any> streamSse(operation: Operation<V, R>, variables: V): Flow<OperationResult<R>> =
        stream(delegate.streamSse(operation, variables))

    fun <V : Any, R : Any> streamWebSocket(operation: Operation<V, R>, variables: V): Flow<OperationResult<R>> =
        stream(delegate.streamWebSocket(operation, variables))

    private fun <R : Any> stream(publisher: JdkFlow.Publisher<OperationResult<R>>): Flow<OperationResult<R>> = callbackFlow {
        val subscription = AtomicReference<JdkFlow.Subscription?>()
        val cancelled = AtomicBoolean()
        fun cancelSubscription() {
            cancelled.set(true)
            subscription.getAndSet(null)?.cancel()
        }
        publisher.subscribe(object : JdkFlow.Subscriber<OperationResult<R>> {
            override fun onSubscribe(value: JdkFlow.Subscription) {
                if (cancelled.get() || !subscription.compareAndSet(null, value)) {
                    value.cancel()
                    return
                }
                if (cancelled.get()) {
                    subscription.compareAndSet(value, null)
                    value.cancel()
                    return
                }
                value.request(Long.MAX_VALUE)
            }

            override fun onNext(item: OperationResult<R>) {
                trySend(item).onFailure { failure ->
                    cancelSubscription()
                    close(failure ?: ClientException(ErrorCode.TRANSPORT))
                }
            }

            override fun onError(failure: Throwable) {
                close(failure)
            }

            override fun onComplete() {
                close()
            }
        })
        awaitClose { cancelSubscription() }
    }
}
