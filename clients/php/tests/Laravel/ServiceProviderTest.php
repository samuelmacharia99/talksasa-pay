<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Laravel;

use Talksasa\Pay\Client;
use Talksasa\Pay\Events\CursorStore;
use Talksasa\Pay\Laravel\Cursor\CacheCursorStore;
use Talksasa\Pay\Laravel\Cursor\DatabaseCursorStore;
use Talksasa\Pay\Laravel\Facades\TalksasaPay;
use Talksasa\Pay\Tests\Support\Fixtures;

final class ServiceProviderTest extends LaravelTestCase
{
    public function testClientIsASingletonAlsoUnderAlias(): void
    {
        $c = $this->app->make(Client::class);
        self::assertSame($c, $this->app->make('talksasa-pay'));
        self::assertSame(Fixtures::BASE_URL . '/v1', $c->baseUrl());
    }

    public function testFacadeCallsTheApi(): void
    {
        $this->http->pushJson(200, Fixtures::whoami());
        self::assertSame('HOST', TalksasaPay::whoami()->accountPrefix);
        self::assertSame('Bearer ' . Fixtures::API_KEY, $this->http->last()->getHeaderLine('Authorization'));
    }

    public function testConfiguredRetriesApply(): void
    {
        config()->set('talksasa-pay.retries', 0);
        $this->http->pushJson(503, ['status' => 503, 'code' => 'overloaded', 'title' => 'x', 'type' => 'about:blank']);
        $this->expectException(\Talksasa\Pay\Exception\ServerException::class);
        try {
            TalksasaPay::whoami();
        } finally {
            self::assertCount(1, $this->http->requests);
        }
    }

    public function testMissingConfigurationIsExplicit(): void
    {
        config()->set('talksasa-pay.api_key', null);
        $this->expectException(\RuntimeException::class);
        $this->expectExceptionMessage('TALKSASA_PAY_API_KEY');
        $this->app->make(Client::class);
    }

    public function testDefaultConfigIsMerged(): void
    {
        self::assertSame(300, config('talksasa-pay.webhook_tolerance'));
        self::assertSame(604800, config('talksasa-pay.dedupe.ttl'));
        self::assertSame('cache', config('talksasa-pay.poller.cursor_store'));
    }

    public function testCursorStoreSelection(): void
    {
        self::assertInstanceOf(CacheCursorStore::class, $this->app->make(CursorStore::class));
        $this->app->forgetInstance(CursorStore::class);
        config()->set('talksasa-pay.poller.cursor_store', 'database');
        self::assertInstanceOf(DatabaseCursorStore::class, $this->app->make(CursorStore::class));
    }

    public function testPublishesConfigAndMigration(): void
    {
        $config = \Illuminate\Support\ServiceProvider::pathsToPublish(null, 'talksasa-pay-config');
        $migrations = \Illuminate\Support\ServiceProvider::pathsToPublish(null, 'talksasa-pay-migrations');
        self::assertCount(1, $config);
        self::assertFileExists((string) array_key_first($config));
        self::assertStringEndsWith('config/talksasa-pay.php', (string) reset($config));
        self::assertCount(1, $migrations);
        self::assertFileExists((string) array_key_first($migrations));
    }

    public function testCommandIsRegistered(): void
    {
        self::assertArrayHasKey('talksasa-pay:poll-events', \Illuminate\Support\Facades\Artisan::all());
    }

    public function testNoSecretsInContainerDump(): void
    {
        $c = $this->app->make(Client::class);
        self::assertStringNotContainsString(Fixtures::API_KEY, print_r($c, true));
    }
}
