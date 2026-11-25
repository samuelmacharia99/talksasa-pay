<?php

declare(strict_types=1);

namespace Talksasa\Pay\Tests\Laravel;

use Illuminate\Http\Request;
use Illuminate\Support\Facades\Event as EventFacade;
use Talksasa\Pay\Exception\AmountTooLargeException;
use Talksasa\Pay\Exception\MpesaErrorException;
use Talksasa\Pay\Exception\MpesaUnavailableException;
use Talksasa\Pay\Exception\NotPayableException;
use Talksasa\Pay\Exception\StkInProgressException;
use Talksasa\Pay\Exception\ValidationException;
use Talksasa\Pay\Laravel\Events\ChargeSucceeded;
use Talksasa\Pay\Laravel\Events\PaymentReceived;
use Talksasa\Pay\Laravel\Events\StkPushFailed;
use Talksasa\Pay\Laravel\Events\TransactionReversed;
use Talksasa\Pay\Laravel\Facades\TalksasaPay;
use Talksasa\Pay\Tests\Support\Fixtures;
use Talksasa\Pay\Webhook\Verifier;

/**
 * The README's "Pay invoice" example, end to end against the mocked API:
 * create a charge with the invoice number as reference, redirect to the
 * checkout page or prompt the phone, mark the invoice paid only on
 * charge.succeeded, and handle payment.received (a duplicate, or a paybill
 * payment the app routes itself) and transaction.reversed.
 */
final class PayInvoiceExampleTest extends LaravelTestCase
{
    /** Stand-in for the app's invoices table. @var array<string,mixed> */
    private static array $invoice = [];
    /** Duplicate payments to refund (receipts). @var list<string> */
    private static array $refunds = [];
    /** Paybill payments nobody could match (for a human). @var list<string> */
    private static array $unapplied = [];

    protected function setUp(): void
    {
        parent::setUp();
        self::$invoice = [
            'id' => 42, 'number' => 'INV-1042', 'customer_account' => 'HOST-1042', 'total_kes' => 1500,
            'status' => 'unpaid', 'charge_id' => null, 'paid_by' => null, 'stk_message' => null,
        ];
        self::$refunds = [];
        self::$unapplied = [];

        // Paid: exactly once, by the transaction that made the charge succeed.
        EventFacade::listen(ChargeSucceeded::class, static function (ChargeSucceeded $e): void {
            if ($e->charge->reference === self::$invoice['number'] && self::$invoice['status'] !== 'paid') {
                self::$invoice['status'] = 'paid';
                self::$invoice['paid_by'] = $e->charge->transactionId;
            }
        });
        // Money that paid no waiting charge.
        EventFacade::listen(PaymentReceived::class, static function (PaymentReceived $e): void {
            $p = $e->payment;
            if ($p->isDuplicate()) {
                self::$refunds[] = (string) $p->receipt; // paid twice: refund it
                return;
            }
            // A paybill payment to the customer's standing account number.
            if ($p->accountNumber === self::$invoice['customer_account']
                && $p->amount === self::$invoice['total_kes'] && self::$invoice['status'] === 'unpaid') {
                self::$invoice['status'] = 'paid';
                self::$invoice['paid_by'] = $p->id;
                return;
            }
            self::$unapplied[] = (string) $p->receipt;
        });
        EventFacade::listen(TransactionReversed::class, static function (TransactionReversed $e): void {
            if ($e->transaction->reversesTransactionId === self::$invoice['paid_by']) {
                self::$invoice['status'] = 'unpaid';
                self::$invoice['paid_by'] = null;
            }
        });
        EventFacade::listen(StkPushFailed::class, static function (StkPushFailed $e): void {
            self::$invoice['stk_message'] = $e->stkPush->resultDesc;
        });
    }

    protected function defineRoutes($router): void
    {
        parent::defineRoutes($router);
        $router->post('invoices/42/pay', static function (Request $request) {
            $inv = self::$invoice;
            if ($inv['status'] === 'paid') {
                return ['view' => 'paid'];
            }
            // One charge per invoice: create it once, then reuse it.
            if ($inv['charge_id'] === null) {
                $charge = TalksasaPay::createCharge(
                    amount: $inv['total_kes'],
                    reference: $inv['number'],
                    description: "Invoice {$inv['number']}",
                    metadata: ['invoice_id' => $inv['id']],
                    idempotencyKey: "invoice-{$inv['id']}-charge",
                );
                self::$invoice['charge_id'] = $charge->id;
            } else {
                $charge = TalksasaPay::getCharge($inv['charge_id']);
            }
            if ($charge->isSucceeded()) {
                return ['view' => 'paid'];
            }

            $phone = (string) $request->input('phone', '');
            if ($phone === '') {
                return redirect()->away($charge->checkoutUrl); // hosted checkout page
            }
            try {
                TalksasaPay::stkPush($charge->id, $phone);
            } catch (StkInProgressException) {
                // A prompt is already on the phone: same screen.
            } catch (ValidationException) {
                return response(['error' => 'Enter a Safaricom number, e.g. 0712 345 678.'], 422);
            } catch (AmountTooLargeException | NotPayableException | MpesaUnavailableException | MpesaErrorException) {
                return ['view' => 'paybill', 'paybill' => $charge->paybill?->businessNumber, 'account' => $charge->paybill?->accountNumber];
            }
            return ['view' => 'check-phone', 'message' => 'Check your phone and enter your M-Pesa PIN.'];
        });
    }

    private function webhook(string $type, array $data, string $id): void
    {
        $body = json_encode(Fixtures::event($type, $data, $id), JSON_THROW_ON_ERROR);
        $this->call('POST', '/webhooks/talksasa-pay', [], [], [], [
            'CONTENT_TYPE' => 'application/json',
            'HTTP_TALKSASA_SIGNATURE' => Verifier::sign($body, self::SECRET),
        ], $body)->assertOk();
    }

    public function testWithoutPhoneRedirectsToCheckout(): void
    {
        $this->http->pushJson(201, Fixtures::charge());

        $this->post('/invoices/42/pay')->assertRedirect('https://pay.talksasa.co.ke/c/tok_abc');

        $r = $this->http->requests[0];
        self::assertSame('/v1/charges', $r->getUri()->getPath());
        self::assertSame('invoice-42-charge', $r->getHeaderLine('Idempotency-Key'));
        self::assertSame(
            ['reference' => 'INV-1042', 'amount' => 1500, 'description' => 'Invoice INV-1042', 'metadata' => ['invoice_id' => 42]],
            json_decode((string) $r->getBody(), true),
        );
        self::assertSame(Fixtures::CHARGE_ID, self::$invoice['charge_id']);
    }

    public function testPromptsPhoneAndMarksPaidOnlyOnChargeSucceeded(): void
    {
        $this->http->pushJson(201, Fixtures::charge());
        $this->http->pushJson(202, Fixtures::stkPush());

        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])
            ->assertOk()->assertJson(['view' => 'check-phone']);

        self::assertSame('/v1/charges/' . Fixtures::CHARGE_ID . '/stk-push', $this->http->requests[1]->getUri()->getPath());
        self::assertNotSame('', $this->http->requests[1]->getHeaderLine('Idempotency-Key'));

        $this->webhook('stk_push.sent', Fixtures::stkPush(), 'e1');
        self::assertSame('unpaid', self::$invoice['status']);

        $this->webhook('stk_push.needs_review', Fixtures::stkPush(['status' => 'needs_review']), 'e2');
        $this->webhook('charge.needs_review', Fixtures::charge(['status' => 'needs_review']), 'e3');
        self::assertSame('unpaid', self::$invoice['status']);

        $this->webhook('stk_push.succeeded', Fixtures::stkPush(['status' => 'succeeded']), 'e4');
        self::assertSame('unpaid', self::$invoice['status']);

        $this->webhook('charge.succeeded', Fixtures::succeededCharge(), 'e5');
        self::assertSame('paid', self::$invoice['status']);
        self::assertSame(Fixtures::TXN_ID, self::$invoice['paid_by']);

        // Redelivery changes nothing.
        $this->webhook('charge.succeeded', Fixtures::succeededCharge(), 'e5');
        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])->assertJson(['view' => 'paid']);
        self::assertCount(2, $this->http->requests);
    }

    public function testFailedPromptKeepsInvoiceUnpaidAndCanPromptAgain(): void
    {
        $this->http->pushJson(201, Fixtures::charge());
        $this->http->pushJson(202, Fixtures::stkPush());
        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])->assertJson(['view' => 'check-phone']);

        $this->webhook('stk_push.failed', Fixtures::stkPush(['status' => 'failed', 'result_code' => '1032', 'result_desc' => 'Request cancelled by user']), 'f1');
        self::assertSame('unpaid', self::$invoice['status']);
        self::assertSame('Request cancelled by user', self::$invoice['stk_message']);

        // Second click: the same charge (still pending) is prompted again with a new key.
        $this->http->pushJson(200, Fixtures::charge());
        $this->http->pushJson(202, Fixtures::stkPush(['id' => '0193a1b2-0000-7000-8000-0000000000dd']));
        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])->assertJson(['view' => 'check-phone']);

        self::assertSame('GET', $this->http->requests[2]->getMethod());
        self::assertSame('/v1/charges/' . Fixtures::CHARGE_ID, $this->http->requests[2]->getUri()->getPath());
        self::assertNotSame(
            $this->http->requests[1]->getHeaderLine('Idempotency-Key'),
            $this->http->requests[3]->getHeaderLine('Idempotency-Key'),
        );
    }

    public function testSecondClickWhilePromptIsLiveShowsCheckYourPhone(): void
    {
        self::$invoice['charge_id'] = Fixtures::CHARGE_ID;
        $this->http->pushJson(200, Fixtures::charge());
        $this->http->pushJson(409, ['title' => 'In progress', 'status' => 409, 'code' => 'stk_in_progress']);

        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])->assertOk()->assertJson(['view' => 'check-phone']);
    }

    public function testFallsBackToPaybillWhenPromptsCannotBeSent(): void
    {
        $this->http->pushJson(201, Fixtures::charge());
        $this->http->pushJson(503, ['title' => 'M-Pesa unavailable', 'status' => 503, 'code' => 'mpesa_unavailable']);

        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])
            ->assertOk()->assertJson(['view' => 'paybill', 'paybill' => '4123456', 'account' => 'HOST-4K7QZ9M']);
        self::assertCount(2, $this->http->requests); // not retried
    }

    public function testInvalidPhone(): void
    {
        $this->http->pushJson(201, Fixtures::charge());
        $this->http->pushJson(422, ['title' => 'Invalid', 'status' => 422, 'code' => 'validation_failed', 'errors' => [['field' => 'phone', 'message' => 'invalid']]]);

        $this->postJson('/invoices/42/pay', ['phone' => '123'])->assertStatus(422);
    }

    public function testLateMoneyAfterCancelStillPaysTheInvoice(): void
    {
        $this->webhook('charge.cancelled', Fixtures::charge(['status' => 'cancelled', 'paybill' => null]), 'l1');
        self::assertSame('unpaid', self::$invoice['status']);
        $this->webhook('charge.succeeded', Fixtures::succeededCharge(), 'l2');
        self::assertSame('paid', self::$invoice['status']);
    }

    public function testDuplicatePaymentIsQueuedForRefund(): void
    {
        $this->webhook('charge.succeeded', Fixtures::succeededCharge(), 'd1');
        $this->webhook('payment.received', Fixtures::paymentReceived([
            'channel' => 'mpesa_stk', 'amount' => 1500, 'receipt' => 'TKR55DUP55',
            'account_number' => 'HOST-4K7QZ9M', 'duplicate_of_charge' => Fixtures::CHARGE_ID,
        ]), 'd2');

        self::assertSame('paid', self::$invoice['status']);
        self::assertSame(Fixtures::TXN_ID, self::$invoice['paid_by']);
        self::assertSame(['TKR55DUP55'], self::$refunds);
    }

    public function testPaybillPaymentRoutedByTheAppPaysTheInvoice(): void
    {
        // Wrong amount: kept for a human, invoice untouched.
        $this->webhook('payment.received', Fixtures::paymentReceived(['receipt' => 'TKR00PART1']), 'p1');
        self::assertSame('unpaid', self::$invoice['status']);
        self::assertSame(['TKR00PART1'], self::$unapplied);

        // Exact amount to the customer's account number: pays it.
        $this->webhook('payment.received', Fixtures::paymentReceived(['id' => 'txn-c2b', 'amount' => 1500, 'receipt' => 'TKR00FULL1']), 'p2');
        self::assertSame('paid', self::$invoice['status']);
        self::assertSame('txn-c2b', self::$invoice['paid_by']);
    }

    public function testReversalReopensTheInvoice(): void
    {
        $this->webhook('charge.succeeded', Fixtures::succeededCharge(), 'r1');
        self::assertSame('paid', self::$invoice['status']);

        // A reversal of some other payment is ignored.
        $this->webhook('transaction.reversed', Fixtures::reversal(['reverses_transaction_id' => 'other']), 'r2');
        self::assertSame('paid', self::$invoice['status']);

        $this->webhook('transaction.reversed', Fixtures::reversal(), 'r3');
        self::assertSame('unpaid', self::$invoice['status']);
        self::assertNull(self::$invoice['paid_by']);
    }
}
