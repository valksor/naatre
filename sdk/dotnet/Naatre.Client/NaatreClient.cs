using System.Diagnostics.CodeAnalysis;
using System.Net.Http;
using System.Runtime.CompilerServices;

namespace Valksor.Naatre.Client;

public interface INaatreClient : IDisposable
{
    Task<OperationResult<TData>> ExecuteAsync<TData>(
        NaatreOperation<TData> operation,
        CancellationToken cancellationToken = default);

    IAsyncEnumerable<StreamEvent<TData>> StreamAsync<TData>(
        NaatreOperation<TData> operation,
        CancellationToken cancellationToken = default);

    IAsyncEnumerable<TPage> PaginateAsync<TPage>(
        Func<string?, NaatreOperation<TPage>> operation,
        Func<TPage, string?> nextCursor,
        CancellationToken cancellationToken = default);
}

public sealed class NaatreAdapterException : Exception
{
    internal NaatreAdapterException(string code)
        : base(code)
    {
        Code = code;
    }

    public string Code { get; }
}

public sealed class NaatreAdapterCanceledException : OperationCanceledException
{
    internal NaatreAdapterCanceledException(CancellationToken cancellationToken)
        : base(ClientErrorCodes.Cancelled, cancellationToken)
    {
    }

    public string Code => ClientErrorCodes.Cancelled;
}

[SuppressMessage("Design", "CA1031:Do not catch general exception types", Justification = "The adapter boundary must not expose handler, credential-provider, parser, or dependency exceptions.")]
public sealed class NaatreClient : INaatreClient
{
    private readonly NaatreHttpClient inner;
    private int disposed;

    public NaatreClient(
        Uri endpoint,
        NaatreHttpOptions? options = null,
        IEnumerable<DelegatingHandler>? handlers = null)
    {
        inner = Create(() => new NaatreHttpClient(endpoint, CopyOptions(options), handlers));
    }

    public NaatreClient(
        Uri endpoint,
        HttpMessageHandler handler,
        NaatreHttpOptions? options = null)
    {
        inner = Create(() => new NaatreHttpClient(endpoint, handler, CopyOptions(options)));
    }

    public async Task<OperationResult<TData>> ExecuteAsync<TData>(
        NaatreOperation<TData> operation,
        CancellationToken cancellationToken = default)
    {
        ThrowIfDisposed();
        if (operation is null)
        {
            throw new NaatreAdapterException(ClientErrorCodes.ConfigInvalid);
        }

        try
        {
            return await inner.ExecuteAsync(operation, cancellationToken).ConfigureAwait(false);
        }
        catch (Exception exception)
        {
            throw PublicFailure(exception, cancellationToken);
        }
    }

    public async IAsyncEnumerable<StreamEvent<TData>> StreamAsync<TData>(
        NaatreOperation<TData> operation,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        ThrowIfDisposed();
        if (operation is null)
        {
            throw new NaatreAdapterException(ClientErrorCodes.ConfigInvalid);
        }

        var enumerator = inner.StreamAsync(operation, cancellationToken).GetAsyncEnumerator(cancellationToken);
        try
        {
            while (await GuardAsync(enumerator.MoveNextAsync, cancellationToken).ConfigureAwait(false))
            {
                yield return enumerator.Current;
            }
        }
        finally
        {
            await GuardAsync(async () =>
            {
                await enumerator.DisposeAsync().ConfigureAwait(false);
                return true;
            }, cancellationToken).ConfigureAwait(false);
        }
    }

    public async IAsyncEnumerable<TPage> PaginateAsync<TPage>(
        Func<string?, NaatreOperation<TPage>> operation,
        Func<TPage, string?> nextCursor,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        ThrowIfDisposed();
        if (operation is null || nextCursor is null)
        {
            throw new NaatreAdapterException(ClientErrorCodes.ConfigInvalid);
        }

        var enumerator = inner.PaginateAsync(operation, nextCursor, cancellationToken).GetAsyncEnumerator(cancellationToken);
        try
        {
            while (await GuardAsync(enumerator.MoveNextAsync, cancellationToken).ConfigureAwait(false))
            {
                yield return enumerator.Current;
            }
        }
        finally
        {
            await GuardAsync(async () =>
            {
                await enumerator.DisposeAsync().ConfigureAwait(false);
                return true;
            }, cancellationToken).ConfigureAwait(false);
        }
    }

    public void Dispose()
    {
        if (Interlocked.Exchange(ref disposed, 1) == 0)
        {
            try
            {
                inner.Dispose();
            }
            catch (Exception exception)
            {
                throw PublicFailure(exception, default);
            }
        }
    }

    private static NaatreHttpClient Create(Func<NaatreHttpClient> factory)
    {
        try
        {
            return factory();
        }
        catch (Exception exception)
        {
            throw PublicFailure(exception, default, ClientErrorCodes.ConfigInvalid);
        }
    }

    private static NaatreHttpOptions CopyOptions(NaatreHttpOptions? options)
    {
        var source = options ?? new NaatreHttpOptions();
        if (source.RedirectOrigins is null || source.CredentialOrigins is null)
        {
            throw new NaatreAdapterException(ClientErrorCodes.ConfigInvalid);
        }

        return source with
        {
            RedirectOrigins = new HashSet<string>(source.RedirectOrigins, StringComparer.Ordinal),
            CredentialOrigins = new HashSet<string>(source.CredentialOrigins, StringComparer.Ordinal),
        };
    }

    private static async ValueTask<TResult> GuardAsync<TResult>(
        Func<ValueTask<TResult>> operation,
        CancellationToken cancellationToken)
    {
        try
        {
            return await operation().ConfigureAwait(false);
        }
        catch (Exception exception)
        {
            throw PublicFailure(exception, cancellationToken);
        }
    }

    private static Exception PublicFailure(
        Exception exception,
        CancellationToken cancellationToken,
        string fallbackCode = ClientErrorCodes.Transport)
    {
        if (exception is NaatreAdapterCanceledException or NaatreAdapterException)
        {
            return exception;
        }

        if (exception is NaatreOperationCanceledException ||
            (exception is OperationCanceledException && cancellationToken.IsCancellationRequested))
        {
            return new NaatreAdapterCanceledException(cancellationToken);
        }

        if (exception is NaatreClientException clientException)
        {
            return new NaatreAdapterException(KnownCode(clientException.Code, fallbackCode));
        }

        return new NaatreAdapterException(fallbackCode);
    }

    private static string KnownCode(string code, string fallbackCode) => code switch
    {
        ClientErrorCodes.Cancelled or
        ClientErrorCodes.ConfigInvalid or
        ClientErrorCodes.ResponseTooLarge or
        ClientErrorCodes.FrameTooLarge or
        ClientErrorCodes.ProtocolInvalid or
        ClientErrorCodes.ResultInvalid or
        ClientErrorCodes.ScalarInvalid or
        ClientErrorCodes.ValuePrecision or
        ClientErrorCodes.StreamTruncated or
        ClientErrorCodes.Transport or
        ClientErrorCodes.Unsupported => code,
        _ => fallbackCode,
    };

    private void ThrowIfDisposed()
    {
        if (Volatile.Read(ref disposed) != 0)
        {
            throw new NaatreAdapterException(ClientErrorCodes.ConfigInvalid);
        }
    }
}
