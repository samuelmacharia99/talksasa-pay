<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel;

use Illuminate\Contracts\Foundation\Application;
use Illuminate\Routing\Route;
use Illuminate\Routing\Router;
use Illuminate\Support\ServiceProvider;
use Talksasa\Pay\Client;
use Talksasa\Pay\Events\CursorStore;
use Talksasa\Pay\Http\RetryPolicy;
use Talksasa\Pay\Laravel\Console\PollEventsCommand;
use Talksasa\Pay\Laravel\Cursor\CacheCursorStore;
use Talksasa\Pay\Laravel\Cursor\DatabaseCursorStore;
use Talksasa\Pay\Laravel\Http\VerifyWebhookSignature;
use Talksasa\Pay\Laravel\Http\WebhookController;

/**
 * Registers the client (also as "talksasa-pay" and the TalksasaPay facade),
 * the webhook route macro and middleware, and the poll command.
 *
 * Optional container bindings (mainly for tests):
 *   "talksasa-pay.http_client"  a PSR-18 client to use instead of Guzzle
 *   "talksasa-pay.sleeper"      a Talksasa\Pay\Http\Sleeper
 */
final class TalksasaPayServiceProvider extends ServiceProvider
{
    private const CONFIG = __DIR__ . '/../../config/talksasa-pay.php';
    private const MIGRATION = __DIR__ . '/../../database/migrations/2026_11_24_000000_create_talksasa_pay_cursors_table.php';

    public function register(): void
    {
        $this->mergeConfigFrom(self::CONFIG, 'talksasa-pay');

        $this->app->singleton(Client::class, static function (Application $app): Client {
            /** @var array<string,mixed> $c */
            $c = $app['config']->get('talksasa-pay', []);
            if (empty($c['base_url']) || empty($c['api_key'])) {
                throw new \RuntimeException('Talksasa Pay is not configured: set TALKSASA_PAY_BASE_URL and TALKSASA_PAY_API_KEY.');
            }

            return new Client(
                apiKey: (string) $c['api_key'],
                baseUrl: (string) $c['base_url'],
                httpClient: $app->bound('talksasa-pay.http_client') ? $app->make('talksasa-pay.http_client') : null,
                retryPolicy: new RetryPolicy(
                    maxRetries: max(0, (int) ($c['retries'] ?? 2)),
                    maxRetryAfter: max(0, (int) ($c['max_retry_after'] ?? 30)),
                ),
                sleeper: $app->bound('talksasa-pay.sleeper') ? $app->make('talksasa-pay.sleeper') : null,
                timeout: (float) ($c['timeout'] ?? 15),
                connectTimeout: (float) ($c['connect_timeout'] ?? 5),
                allowInsecureHttp: (bool) ($c['allow_insecure_http'] ?? false),
            );
        });
        $this->app->alias(Client::class, 'talksasa-pay');

        $this->app->singleton(EventRelay::class, static function (Application $app): EventRelay {
            $c = $app['config']->get('talksasa-pay.dedupe', []);
            return new EventRelay(
                $app['cache']->store($c['store'] ?? null),
                $app['events'],
                (int) ($c['ttl'] ?? 604800),
                (string) ($c['prefix'] ?? 'talksasa-pay:event:'),
            );
        });

        $this->app->singleton(CursorStore::class, static function (Application $app): CursorStore {
            $c = $app['config']->get('talksasa-pay.poller', []);
            return match ($c['cursor_store'] ?? 'cache') {
                'database' => new DatabaseCursorStore(
                    $app['db']->connection($c['connection'] ?? null),
                    (string) ($c['table'] ?? 'talksasa_pay_cursors'),
                    (string) ($c['name'] ?? 'events'),
                ),
                'cache' => new CacheCursorStore(
                    $app['cache']->store($c['cache_store'] ?? null),
                    (string) ($c['cache_key'] ?? 'talksasa-pay:events-cursor'),
                ),
                default => throw new \InvalidArgumentException('talksasa-pay.poller.cursor_store must be "database" or "cache".'),
            };
        });
    }

    public function boot(): void
    {
        /** @var Router $router */
        $router = $this->app['router'];
        $router->aliasMiddleware('talksasa-pay.webhook', VerifyWebhookSignature::class);

        /*
         * Route::talksasaPayWebhook('webhooks/talksasa-pay') registers
         * POST <uri> -> WebhookController behind the signature check, with
         * CSRF protection removed (Talksasa cannot send a CSRF token; the
         * signature is the authentication).
         */
        Router::macro('talksasaPayWebhook', function (string $uri = 'talksasa-pay/webhook'): Route {
            /** @var Router $this */
            return $this->post($uri, WebhookController::class)
                ->middleware(VerifyWebhookSignature::class)
                ->withoutMiddleware([
                    // Laravel 13+ name; subclasses (incl. an app's own VerifyCsrfToken) are excluded too.
                    'Illuminate\Foundation\Http\Middleware\PreventRequestForgery',
                    \Illuminate\Foundation\Http\Middleware\VerifyCsrfToken::class,
                    \Illuminate\Foundation\Http\Middleware\ValidateCsrfToken::class,
                ])
                ->name('talksasa-pay.webhook');
        });

        if ($this->app->runningInConsole()) {
            $this->publishes([self::CONFIG => config_path('talksasa-pay.php')], 'talksasa-pay-config');
            $this->publishes([
                self::MIGRATION => database_path('migrations/2026_11_24_000000_create_talksasa_pay_cursors_table.php'),
            ], 'talksasa-pay-migrations');
            $this->commands([PollEventsCommand::class]);
        }
    }
}
