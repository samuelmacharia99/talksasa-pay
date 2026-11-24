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
use Talksasa\Pay\Laravel\Events\PaymentRequestPaid;
use Talksasa\Pay\Laravel\Events\StkPushFailed;
use Talksasa\Pay\Laravel\Facades\TalksasaPay;
use Talksasa\Pay\Tests\Support\Fixtures;
use Talksasa\Pay\Webhook\Verifier;

/**
 * The README's "Pay invoice button" example, end to end against the mocked
 * API: the invoice is marked paid only by payment_request.paid, never by
 * stk_push.sent or stk_push.needs_review.
 */
final class PayInvoiceExampleTest extends LaravelTestCase
{
    /** Stand-in for the app's invoices table. @var array<string,mixed> */
    private static array $invoice = [];

    protected function setUp(): void
    {
        parent::setUp();
        self::$invoice = ['id' => 42, 'number' => 'INV-1042', 'user_id' => 'cust-77', 'total_kes' => 1500, 'status' => 'unpaid', 'stk_message' => null];

        EventFacade::listen(PaymentRequestPaid::class, static function (PaymentRequestPaid $e): void {
            if ($e->paymentRequest->reference === self::$invoice['number'] && self::$invoice['status'] !== 'paid') {
                self::$invoice['status'] = 'paid';
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
            $pr = TalksasaPay::createPaymentRequest(
                payer: $inv['user_id'],
                reference: $inv['number'],
                description: "Invoice {$inv['number']}",
                amount: $inv['total_kes'],
                idempotencyKey: "invoice-{$inv['id']}-v1",
            );
            if ($pr->isPaid()) {
                return ['view' => 'paid'];
            }
            try {
                TalksasaPay::stkPush($pr->id, (string) $request->input('phone'));
            } catch (StkInProgressException) {
                // A prompt is already on the phone: same screen.
            } catch (ValidationException) {
                return response(['error' => 'Enter a Safaricom number, e.g. 0712 345 678.'], 422);
            } catch (AmountTooLargeException | NotPayableException | MpesaUnavailableException | MpesaErrorException) {
                return ['view' => 'paybill', 'paybill' => $pr->paybill?->businessNumber, 'account' => $pr->paybill?->accountNumber];
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

    public function testHappyPathMarksPaidOnlyOnPaymentRequestPaid(): void
    {
        $this->http->pushJson(201, Fixtures::paymentRequest(['amount_paid' => 0, 'outstanding' => 1500, 'status' => 'open']));
        $this->http->pushJson(202, Fixtures::stkPush(['amount' => 1500]));

        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])
            ->assertOk()->assertJson(['view' => 'check-phone']);

        self::assertSame('invoice-42-v1', $this->http->requests[0]->getHeaderLine('Idempotency-Key'));
        self::assertSame('/v1/payment-requests/' . Fixtures::REQUEST_ID . '/stk-push', $this->http->requests[1]->getUri()->getPath());
        self::assertNotSame('', $this->http->requests[1]->getHeaderLine('Idempotency-Key'));

        $this->webhook('stk_push.sent', Fixtures::stkPush(), 'e1');
        self::assertSame('unpaid', self::$invoice['status']);

        $this->webhook('stk_push.needs_review', Fixtures::stkPush(['status' => 'needs_review']), 'e2');
        self::assertSame('unpaid', self::$invoice['status']);

        $this->webhook('stk_push.succeeded', Fixtures::stkPush(['status' => 'succeeded']), 'e3');
        self::assertSame('unpaid', self::$invoice['status']);

        $this->webhook('payment_request.paid', Fixtures::paymentRequest(['status' => 'paid', 'amount_paid' => 1500, 'outstanding' => 0, 'paybill' => null]), 'e4');
        self::assertSame('paid', self::$invoice['status']);
    }

    public function testFailedPromptKeepsInvoiceUnpaidAndRecordsReason(): void
    {
        $this->webhook('stk_push.failed', Fixtures::stkPush(['status' => 'failed', 'result_code' => '1032', 'result_desc' => 'Request cancelled by user']), 'f1');
        self::assertSame('unpaid', self::$invoice['status']);
        self::assertSame('Request cancelled by user', self::$invoice['stk_message']);
    }

    public function testFallsBackToPaybillWhenPromptsCannotBeSent(): void
    {
        $this->http->pushJson(200, Fixtures::paymentRequest());
        $this->http->pushJson(503, ['title' => 'M-Pesa unavailable', 'status' => 503, 'code' => 'mpesa_unavailable']);

        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])
            ->assertOk()->assertJson(['view' => 'paybill', 'paybill' => '4123456', 'account' => 'HOST-1042']);
        self::assertCount(2, $this->http->requests); // not retried
    }

    public function testSecondClickWhilePromptIsLiveShowsCheckYourPhone(): void
    {
        $this->http->pushJson(200, Fixtures::paymentRequest());
        $this->http->pushJson(409, ['title' => 'In progress', 'status' => 409, 'code' => 'stk_in_progress']);

        $this->postJson('/invoices/42/pay', ['phone' => '0712345678'])->assertOk()->assertJson(['view' => 'check-phone']);
    }

    public function testInvalidPhone(): void
    {
        $this->http->pushJson(200, Fixtures::paymentRequest());
        $this->http->pushJson(422, ['title' => 'Invalid', 'status' => 422, 'code' => 'validation_failed', 'errors' => [['field' => 'phone', 'message' => 'invalid']]]);

        $this->postJson('/invoices/42/pay', ['phone' => '123'])->assertStatus(422);
    }
}
