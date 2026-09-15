using Microsoft.Extensions.Http;
using Valksor.Naatre;
using Valksor.Naatre.Client;

namespace Microsoft.Extensions.DependencyInjection;

public static class NaatreServiceCollectionExtensions
{
    public const string DefaultClientName = "Valksor.Naatre";

    public static IHttpClientBuilder AddNaatreClient(
        this IServiceCollection services,
        Uri endpoint,
        NaatreHttpOptions? options = null) =>
        AddNaatreClient(services, DefaultClientName, endpoint, options);

    public static IHttpClientBuilder AddNaatreClient(
        this IServiceCollection services,
        string name,
        Uri endpoint,
        NaatreHttpOptions? options = null)
    {
        ArgumentNullException.ThrowIfNull(services);
        ArgumentException.ThrowIfNullOrWhiteSpace(name);
        ArgumentNullException.ThrowIfNull(endpoint);

        var builder = services.AddHttpClient(name);
        services.AddTransient<INaatreClient>(provider =>
        {
            var handlers = provider.GetRequiredService<IHttpMessageHandlerFactory>();
            return new NaatreClient(endpoint, handlers.CreateHandler(name), options);
        });
        return builder;
    }
}
