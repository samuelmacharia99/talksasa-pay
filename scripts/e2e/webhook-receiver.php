<?php
// Local webhook receiver for scripts/e2e-mpesa.sh. Runs with PHP's built-in
// server INSIDE the worker container's network namespace (127.0.0.1), and
// verifies every delivery with the real PHP SDK (clients/php, mounted at
// /sdk): signature + timestamp via Talksasa\Pay\Webhook\Verifier, then the
// typed Event model. Verified events are appended to /out/events.jsonl;
// anything that fails verification is answered 400 and logged to
// /out/rejected.jsonl. The signing secret is read from a file and never
// printed.
declare(strict_types=1);

spl_autoload_register(static function (string $class): void {
    $prefix = 'Talksasa\\Pay\\';
    if (str_starts_with($class, $prefix)) {
        $file = '/sdk/src/' . str_replace('\\', '/', substr($class, strlen($prefix))) . '.php';
        if (is_file($file)) {
            require $file;
        }
    }
});

use Talksasa\Pay\Model\PaymentRequest;
use Talksasa\Pay\Model\StkPush;
use Talksasa\Pay\Model\Transaction;
use Talksasa\Pay\Webhook\Verifier;
use Talksasa\Pay\Webhook\WebhookException;

if (($_SERVER['REQUEST_METHOD'] ?? '') === 'GET') {
    echo "ok\n"; // readiness probe
    return true;
}
$secret = trim((string) file_get_contents('/run/webhook_secret'));
$body = (string) file_get_contents('php://input');
$sig = $_SERVER['HTTP_TALKSASA_SIGNATURE'] ?? '';
try {
    $event = Verifier::verify($body, $sig, $secret);
} catch (WebhookException $e) {
    file_put_contents('/out/rejected.jsonl', json_encode(['reason' => $e->reason, 'header_id' => $_SERVER['HTTP_TALKSASA_EVENT_ID'] ?? null]) . "\n", FILE_APPEND | LOCK_EX);
    http_response_code(400);
    return true;
}
$rec = ['id' => $event->id, 'type' => $event->type, 'header_id' => $_SERVER['HTTP_TALKSASA_EVENT_ID'] ?? null];
$d = $event->data;
if ($d instanceof PaymentRequest) {
    $rec += ['request_id' => $d->id, 'reference' => $d->reference, 'status' => $d->status, 'amount_paid' => $d->amountPaid];
} elseif ($d instanceof StkPush) {
    $rec += ['request_id' => $d->paymentRequestId, 'push_id' => $d->id, 'status' => $d->status,
        'transaction_id' => $d->transactionId, 'result_code' => $d->resultCode, 'cancelled' => $d->wasCancelledByCustomer()];
} elseif ($d instanceof Transaction) {
    $rec += ['transaction_id' => $d->id, 'status' => $d->status, 'receipt' => $d->receipt, 'channel' => $d->channel];
}
file_put_contents('/out/events.jsonl', json_encode($rec) . "\n", FILE_APPEND | LOCK_EX);
http_response_code(200);
echo '{"received":true}';
return true;
