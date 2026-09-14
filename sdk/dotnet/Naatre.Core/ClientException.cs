namespace Valksor.Naatre;

public static class ClientErrorCodes
{
    public const string Cancelled = "CLIENT_CANCELLED";
    public const string ConfigInvalid = "CLIENT_CONFIG_INVALID";
    public const string ResponseTooLarge = "CLIENT_RESPONSE_TOO_LARGE";
    public const string FrameTooLarge = "CLIENT_FRAME_TOO_LARGE";
    public const string ProtocolInvalid = "CLIENT_PROTOCOL_INVALID";
    public const string ResultInvalid = "CLIENT_RESULT_INVALID";
    public const string ScalarInvalid = "CLIENT_SCALAR_INVALID";
    public const string ValuePrecision = "CLIENT_VALUE_PRECISION";
    public const string StreamTruncated = "CLIENT_STREAM_TRUNCATED";
    public const string Transport = "CLIENT_TRANSPORT_ERROR";
    public const string Unsupported = "CLIENT_UNSUPPORTED";
}

public sealed class NaatreClientException : Exception
{
    public NaatreClientException(string code, Exception? innerException = null)
        : base(code, innerException)
    {
        Code = code;
    }

    public string Code { get; }
}

public sealed class NaatreOperationCanceledException : OperationCanceledException
{
    private readonly string code = ClientErrorCodes.Cancelled;

    public NaatreOperationCanceledException(CancellationToken cancellationToken, Exception? innerException = null)
        : base(ClientErrorCodes.Cancelled, innerException, cancellationToken)
    {
    }

    public string Code => code;
}
