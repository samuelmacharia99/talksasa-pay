<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Support;

/** Resource JSON as api/app.openapi.yaml defines it (plus an unknown field to prove it is ignored). */
final class Fixtures
{
    public const API_KEY = 'tsp_test_k1a2b3_SuperSecretValue0123456789';
    public const BASE_URL = 'https://pay-api.example.test';
    public const CHARGE_ID = '0193a1b2-0000-7000-8000-000000000001';
    public const TXN_ID = '0193a1b2-0000-7000-8000-0000000000aa';
    public const STK_ID = '0193a1b2-0000-7000-8000-0000000000cc';

    /** @return array<string,mixed> */
    public static function whoami(): array
    {
        return ['app_id' => '0193a1b2-0000-7000-8000-0000000000a1', 'app_name' => 'Cloud Hosting', 'account_prefix' => 'HOST', 'key_id' => 'k1a2b3'];
    }

    /** @return array<string,mixed> */
    public static function charge(array $override = []): array
    {
        return $override + [
            'object' => 'charge',
            'id' => self::CHARGE_ID,
            'reference' => 'INV-1042',
            'amount' => 1500,
            'currency' => 'KES',
            'description' => 'Cloud hosting, December 2026',
            'metadata' => ['invoice_id' => 42, 'customer' => 'cust-77'],
            'account_number' => 'HOST-4K7QZ9M',
            'status' => 'pending',
            'checkout_url' => 'https://pay.talksasa.co.ke/c/tok_abc',
            'paybill' => ['business_number' => '4123456', 'account_number' => 'HOST-4K7QZ9M'],
            'expires_at' => '2026-11-30T08:00:00Z',
            'succeeded_at' => null,
            'transaction_id' => null,
            'latest_stk_push' => null,
            'created_at' => '2026-11-29T08:00:00.123456789Z',
            'updated_at' => '2026-11-29T08:00:00Z',
            'unknown' => 'ignored',
        ];
    }

    /** @return array<string,mixed> A succeeded charge, as charge.succeeded carries it. */
    public static function succeededCharge(array $override = []): array
    {
        return self::charge($override + [
            'status' => 'succeeded',
            'paybill' => null,
            'succeeded_at' => '2026-11-30T06:15:01Z',
            'transaction_id' => self::TXN_ID,
            'latest_stk_push' => self::stkPush(['status' => 'succeeded', 'completed_at' => '2026-11-30T06:15:01Z']),
        ]);
    }

    /** @return array<string,mixed> */
    public static function transaction(array $override = []): array
    {
        return $override + [
            'object' => 'transaction',
            'id' => self::TXN_ID,
            'kind' => 'payment',
            'direction' => 'in',
            'channel' => 'mpesa_stk',
            'status' => 'succeeded',
            'amount' => 1500,
            'currency' => 'KES',
            'receipt' => 'TKR12ABC34',
            'phone' => '2547****5678',
            'account_number' => 'HOST-4K7QZ9M',
            'charge_id' => self::CHARGE_ID,
            'reverses_transaction_id' => null,
            'occurred_at' => '2026-11-30T09:15:00+03:00',
            'created_at' => '2026-11-30T06:15:01Z',
            'completed_at' => '2026-11-30T06:15:01Z',
        ];
    }

    /** @return array<string,mixed> A paybill payment that matched no waiting charge. */
    public static function paymentReceived(array $override = []): array
    {
        return self::transaction($override + [
            'id' => '0193a1b2-0000-7000-8000-0000000000bb',
            'channel' => 'mpesa_c2b',
            'amount' => 700,
            'receipt' => 'TKR99XYZ00',
            'account_number' => 'HOST-1042',
            'charge_id' => null,
            'duplicate_of_charge' => null,
        ]);
    }

    /** @return array<string,mixed> The reversal transaction of transaction.reversed. */
    public static function reversal(array $override = []): array
    {
        return self::transaction($override + [
            'id' => '0193a1b2-0000-7000-8000-0000000000ee',
            'kind' => 'reversal',
            'direction' => 'out',
            'channel' => 'manual',
            'receipt' => null,
            'reverses_transaction_id' => self::TXN_ID,
        ]);
    }

    /** @return array<string,mixed> */
    public static function stkPush(array $override = []): array
    {
        return $override + [
            'object' => 'stk_push',
            'id' => self::STK_ID,
            'charge_id' => self::CHARGE_ID,
            'transaction_id' => self::TXN_ID,
            'status' => 'sent',
            'amount' => 1500,
            'phone' => '2547****5678',
            'checkout_request_id' => 'ws_CO_241120261015001234',
            'result_code' => null,
            'result_desc' => null,
            'created_at' => '2026-11-30T06:15:00Z',
            'updated_at' => '2026-11-30T06:15:01.25Z',
            'completed_at' => null,
            'future_field' => 1,
        ];
    }

    /** @return array<string,mixed> Typical data for each event type. */
    public static function dataFor(string $type): array
    {
        return match (true) {
            $type === 'charge.succeeded' => self::succeededCharge(),
            $type === 'charge.needs_review' => self::charge(['status' => 'needs_review']),
            $type === 'charge.expired' => self::charge(['status' => 'expired', 'paybill' => null]),
            $type === 'charge.cancelled' => self::charge(['status' => 'cancelled', 'paybill' => null]),
            $type === 'payment.received' => self::paymentReceived(),
            $type === 'transaction.reversed' => self::reversal(),
            str_starts_with($type, 'stk_push.') => self::stkPush(['status' => substr($type, strlen('stk_push.'))]),
            default => ['whatever' => 1],
        };
    }

    /** @return array<string,mixed> */
    public static function event(string $type = 'charge.succeeded', ?array $data = null, string $id = '0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b'): array
    {
        return [
            'object' => 'event',
            'id' => $id,
            'type' => $type,
            'api_version' => '2026-11-25',
            'created_at' => '2026-11-30T06:15:01.000001Z',
            'data' => $data ?? self::dataFor($type),
        ];
    }
}
