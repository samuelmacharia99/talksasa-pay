<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Laravel;

use Illuminate\Routing\Router;
use Orchestra\Testbench\TestCase;
use Talksasa\Pay\Laravel\TalksasaPayServiceProvider;
use Talksasa\Pay\Tests\Support\FakeSleeper;
use Talksasa\Pay\Tests\Support\Fixtures;
use Talksasa\Pay\Tests\Support\MockHttpClient;

abstract class LaravelTestCase extends TestCase
{
    protected const SECRET = 'whsec_laravel_test_secret';
    protected const OLD_SECRET = 'whsec_laravel_previous_secret';

    protected MockHttpClient $http;

    protected function setUp(): void
    {
        $this->http = new MockHttpClient();
        parent::setUp();
        $this->app->instance('talksasa-pay.http_client', $this->http);
        $this->app->instance('talksasa-pay.sleeper', new FakeSleeper());
    }

    protected function getPackageProviders($app): array
    {
        return [TalksasaPayServiceProvider::class];
    }

    protected function getPackageAliases($app): array
    {
        return ['TalksasaPay' => \Talksasa\Pay\Laravel\Facades\TalksasaPay::class];
    }

    protected function defineEnvironment($app): void
    {
        $app['config']->set('app.key', 'base64:' . base64_encode(str_repeat('k', 32)));
        $app['config']->set('cache.default', 'array');
        $app['config']->set('talksasa-pay.base_url', Fixtures::BASE_URL);
        $app['config']->set('talksasa-pay.api_key', Fixtures::API_KEY);
        $app['config']->set('talksasa-pay.webhook_secrets', [self::SECRET, self::OLD_SECRET]);
    }

    /** @param Router $router */
    protected function defineRoutes($router): void
    {
        // As an app would in routes/web.php (CSRF is removed by the macro).
        $router->middleware('web')->group(function () use ($router): void {
            $router->talksasaPayWebhook('webhooks/talksasa-pay');
            $router->post('csrf-probe', static fn () => 'ok')->name('csrf-probe');
        });
    }
}
