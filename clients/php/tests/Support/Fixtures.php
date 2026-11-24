<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Support;

/** Resource JSON exactly as internal/apiview renders it (plus an unknown field to prove it is ignored). */
final class Fixtures
{
    public const API_KEY = 'tsp_test_k1a2b3_SuperSecretValue0123456789';
    public const BASE_URL = 'https://pay-api.example.test';
    public const REQUEST_ID = '0193a1b2-0000-7000-8000-000000000001';
    public const TXN_ID = '0193a1b2-0000-7000-8000-0000000000aa';

    /** @return array<string,mixed> */
    public static function payer(array $override = []): array
    {
        return $override + [
            'object' => 'payer',
            'id' => '0193a1b2-0000-7000-8000-0000000000ff',
            'external_ref' => 'cust-77',
            'account_number' => 'HOST-1042',
            'display_name' => 'Wanjiku Kamau',
            'email' => 'wanjiku@example.com',
            'phones' => ['2547****5678'],
            'status' => 'active',
            'credit_balance' => 200,
            'created_at' => '2026-11-01T10:00:00Z',
            'updated_at' => '2026-11-02T10:00:00.5Z',
            'some_future_field' => ['ignored' => true],
        ];
    }

    /** @return array<string,mixed> */
    public static function paymentRequest(array $override = []): array
    {
        return $override + [
            'object' => 'payment_request',
            'id' => self::REQUEST_ID,
            'reference' => 'INV-1042',
            'description' => 'Cloud hosting, December 2026',
            'currency' => 'KES',
            'amount' => 1500,
            'amount_paid' => 200,
            'outstanding' => 1300,
            'status' => 'partially_paid',
            'payer' => ['external_ref' => 'cust-77', 'account_number' => 'HOST-1042'],
            'pay_url' => 'https://pay.talksasa.co.ke/tok_abc',
            'paybill' => ['business_number' => '4123456', 'account_number' => 'HOST-1042'],
            'expires_at' => null,
            'created_at' => '2026-11-29T08:00:00.123456789Z',
            'updated_at' => '2026-11-29T08:00:00Z',
            'paid_at' => null,
            'closed_at' => null,
            'unknown' => 'ignored',
        ];
    }

    /** @return array<string,mixed> */
    public static function transaction(array $override = []): array
    {
        return $override + [
            'object' => 'transaction',
            'id' => self::TXN_ID,
            'kind' => 'payment',
            'direction' => 'in',
            'channel' => 'mpesa_c2b',
            'status' => 'succeeded',
            'amount' => 2000,
            'currency' => 'KES',
            'receipt' => 'TKR12ABC34',
            'phone' => '2547****5678',
            'payer' => ['external_ref' => 'cust-77', 'account_number' => 'HOST-1042'],
            'payment_request_id' => null,
            'reverses_transaction_id' => null,
            'allocations' => [
                ['payment_request_id' => self::REQUEST_ID, 'amount' => 1300, 'kind' => 'payment'],
                ['payment_request_id' => null, 'amount' => 700, 'kind' => 'payment'],
            ],
            'occurred_at' => '2026-11-30T09:15:00+03:00',
            'created_at' => '2026-11-30T06:15:01Z',
            'completed_at' => '2026-11-30T06:15:01Z',
        ];
    }

    /** @return array<string,mixed> */
    public static function event(string $type = 'payment_request.paid', ?array $data = null, string $id = '0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b'): array
    {
        $data ??= match (true) {
            str_starts_with($type, 'transaction.') => self::transaction(),
            $type === 'payer.credit_changed' => [
                'payer' => ['external_ref' => 'cust-77', 'account_number' => 'HOST-1042'],
                'delta' => 700,
                'credit_balance' => 900,
                'transaction_id' => self::TXN_ID,
            ],
            default => self::paymentRequest(['status' => 'paid', 'amount_paid' => 1500, 'outstanding' => 0, 'paybill' => null]),
        };

        return [
            'object' => 'event',
            'id' => $id,
            'type' => $type,
            'api_version' => '2026-11-24',
            'created_at' => '2026-11-30T06:15:01.000001Z',
            'data' => $data,
        ];
    }
}
