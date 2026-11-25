<?php
// Local webhook receiver for scripts/e2e-mpesa.sh. Runs with PHP's built-in
// server INSIDE the worker container's network namespace (127.0.0.1), and
// verifies every delivery with the real PHP SDK (clients/php, mounted at
// /sdk): signature + timestamp + event envelope via
// Talksasa\Pay\Webhook\Verifier::verify. The event's data is then read
// generically from the verified raw body (no typed models), flattened to
// the fields the test checks and appended to /out/events.jsonl. Anything
// that fails verification is answered 400 and logged to
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

// Generic parsing: the envelope was verified above; read data as plain JSON.
$env = json_decode($body, true, 64, JSON_THROW_ON_ERROR);
$d = is_array($env['data'] ?? null) ? $env['data'] : [];
$obj = $d['object'] ?? null;
$rec = [
    'id' => $event->id,
    'type' => $event->type,
    'header_id' => $_SERVER['HTTP_TALKSASA_EVENT_ID'] ?? null,
    'object' => $obj,
    'status' => $d['status'] ?? null,
    'amount' => $d['amount'] ?? null,
    'account_number' => $d['account_number'] ?? null,
];
if ($obj === 'charge') {
    $rec += ['charge_id' => $d['id'] ?? null, 'reference' => $d['reference'] ?? null,
        'transaction_id' => $d['transaction_id'] ?? null];
} elseif ($obj === 'stk_push') {
    $rec += ['push_id' => $d['id'] ?? null, 'charge_id' => $d['charge_id'] ?? null,
        'transaction_id' => $d['transaction_id'] ?? null, 'result_code' => $d['result_code'] ?? null];
} elseif ($obj === 'transaction') {
    $rec += ['transaction_id' => $d['id'] ?? null, 'charge_id' => $d['charge_id'] ?? null,
        'receipt' => $d['receipt'] ?? null, 'channel' => $d['channel'] ?? null, 'kind' => $d['kind'] ?? null,
        'reverses_transaction_id' => $d['reverses_transaction_id'] ?? null,
        'duplicate_of_charge' => $d['duplicate_of_charge'] ?? null];
}
file_put_contents('/out/events.jsonl', json_encode($rec) . "\n", FILE_APPEND | LOCK_EX);
http_response_code(200);
echo '{"received":true}';
return true;
