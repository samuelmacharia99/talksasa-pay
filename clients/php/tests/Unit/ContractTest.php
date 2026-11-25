<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Unit;

use PHPUnit\Framework\TestCase;
use Talksasa\Pay\Client;
use Talksasa\Pay\Exception\ApiException;
use Talksasa\Pay\Laravel\EventRelay;
use Talksasa\Pay\Laravel\Facades\TalksasaPay;
use Talksasa\Pay\Model\Event;
use Talksasa\Pay\Model\EventType;
use Talksasa\Pay\Tests\Support\Fixtures;

/**
 * Keeps the SDK in step with api/app.openapi.yaml: every operation has a
 * client method (and facade entry), every event type a model and a Laravel
 * event, and every problem code an exception class. Skipped when the SDK is
 * used outside the Talksasa Pay repository.
 */
final class ContractTest extends TestCase
{
    /** operationId => Client method. */
    private const METHODS = [
        'whoami' => 'whoami',
        'createCharge' => 'createCharge',
        'listCharges' => 'listCharges',
        'getCharge' => 'getCharge',
        'sendSTKPush' => 'stkPush',
        'cancelCharge' => 'cancelCharge',
        'getSTKPush' => 'getStkPush',
        'listTransactions' => 'listTransactions',
        'getTransaction' => 'getTransaction',
        'listEvents' => 'listEvents',
    ];

    private static function spec(): string
    {
        $path = __DIR__ . '/../../../../api/app.openapi.yaml';
        if (!is_file($path)) {
            self::markTestSkipped('api/app.openapi.yaml not found (SDK used outside its repository).');
        }
        return (string) file_get_contents($path);
    }

    /** @return list<string> the items of the YAML list directly under "<$key>:\n  enum:" */
    private static function enumAfter(string $spec, string $anchor): array
    {
        $pos = strpos($spec, $anchor);
        self::assertNotFalse($pos, "spec has no {$anchor}");
        $rest = substr($spec, $pos + strlen($anchor));
        $enum = strpos($rest, 'enum:');
        self::assertNotFalse($enum);
        $lines = explode("\n", substr($rest, $enum + strlen('enum:')));
        array_shift($lines);
        $items = [];
        foreach ($lines as $line) {
            if (preg_match('/^\s+- ([a-z_.]+)\s*$/', $line, $m) !== 1) {
                break;
            }
            $items[] = $m[1];
        }
        self::assertNotEmpty($items, "empty enum after {$anchor}");
        return $items;
    }

    public function testEveryOperationHasAClientMethod(): void
    {
        preg_match_all('/operationId:\s*(\w+)/', self::spec(), $m);
        $ops = $m[1];
        self::assertNotEmpty($ops);
        sort($ops);
        $known = array_keys(self::METHODS);
        sort($known);
        self::assertSame($known, $ops, 'operations in the spec and in ContractTest::METHODS differ');

        $facade = (string) (new \ReflectionClass(TalksasaPay::class))->getDocComment();
        foreach (self::METHODS as $op => $method) {
            self::assertTrue(method_exists(Client::class, $method), "{$op}: Client::{$method}() missing");
            self::assertTrue((new \ReflectionMethod(Client::class, $method))->isPublic());
            self::assertStringContainsString(" {$method}(", $facade, "{$op}: facade lacks @method {$method}");
        }
    }

    public function testEveryFacadeMethodExists(): void
    {
        preg_match_all('/@method static \S+ (\w+)\(/', (string) (new \ReflectionClass(TalksasaPay::class))->getDocComment(), $m);
        foreach ($m[1] as $method) {
            self::assertTrue(method_exists(Client::class, $method), "facade documents missing Client::{$method}()");
        }
    }

    public function testEveryEventTypeHasAModelAndALaravelEvent(): void
    {
        $types = self::enumAfter(self::spec(), "    EventType:\n");
        sort($types);
        $all = EventType::ALL;
        sort($all);
        self::assertSame($types, $all, 'EventType::ALL differs from the spec');

        foreach ($types as $type) {
            self::assertArrayHasKey($type, Event::DATA_CLASSES, "{$type}: no model");
            self::assertArrayHasKey($type, EventRelay::CLASSES, "{$type}: no Laravel event");
            $event = Event::fromArray(Fixtures::event($type));
            self::assertInstanceOf(Event::DATA_CLASSES[$type], $event->data, $type);
            $laravel = EventRelay::CLASSES[$type];
            self::assertInstanceOf($laravel, new $laravel($event, 'webhook'));
        }
    }

    public function testEveryProblemCodeHasAnException(): void
    {
        $codes = self::enumAfter(self::spec(), "        code:\n");
        $mapped = array_keys(ApiException::CLASSES_BY_CODE);
        sort($codes);
        sort($mapped);
        self::assertSame($codes, $mapped, 'problem codes in the spec and ApiException::CLASSES_BY_CODE differ');
        foreach (ApiException::CLASSES_BY_CODE as $class) {
            self::assertTrue(is_subclass_of($class, ApiException::class), $class);
        }
    }
}
