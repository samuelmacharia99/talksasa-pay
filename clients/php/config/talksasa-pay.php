<?php

declare(strict_types=1);

return [

    /*
    | Talksasa Pay API base URL (the client appends /v1 if missing). Must be
    | https unless the host is loopback or allow_insecure_http is true.
    */
    'base_url' => env('TALKSASA_PAY_BASE_URL'),

    /*
    | App API key: tsp_live_... in production, tsp_test_... elsewhere.
    | Keep it in .env only; never commit it, never log it.
    */
    'api_key' => env('TALKSASA_PAY_API_KEY'),

    /*
    | Webhook signing secrets (whsec_...). During a rotation Talksasa signs
    | with both the new and the old secret for 24 h: put the new one in
    | TALKSASA_PAY_WEBHOOK_SECRET and the old one in
    | TALKSASA_PAY_WEBHOOK_SECRET_PREVIOUS until the grace period ends.
    */
    'webhook_secrets' => array_values(array_filter([
        env('TALKSASA_PAY_WEBHOOK_SECRET'),
        env('TALKSASA_PAY_WEBHOOK_SECRET_PREVIOUS'),
    ])),

    // Maximum age (either direction) of a webhook's signed timestamp, seconds.
    'webhook_tolerance' => (int) env('TALKSASA_PAY_WEBHOOK_TOLERANCE', 300),

    // Seconds per HTTP attempt.
    'timeout' => (float) env('TALKSASA_PAY_TIMEOUT', 15),
    'connect_timeout' => (float) env('TALKSASA_PAY_CONNECT_TIMEOUT', 5),

    // Automatic retries on network errors, 429, 502, 503, 504 (all /v1 calls are idempotent).
    'retries' => (int) env('TALKSASA_PAY_RETRIES', 2),

    // Longest Retry-After the client will wait out inline; longer -> exception.
    'max_retry_after' => (int) env('TALKSASA_PAY_MAX_RETRY_AFTER', 30),

    // Development only: allow http:// to a non-loopback host (e.g. a docker service name).
    'allow_insecure_http' => (bool) env('TALKSASA_PAY_ALLOW_INSECURE_HTTP', false),

    /*
    | De-duplication of events by id, shared by webhooks and the poller so a
    | listener sees each event once even when both deliver it. Uses the
    | Laravel cache (Cache::add is atomic); choose a store shared by all app
    | servers (redis, database, memcached). Listeners should still be
    | idempotent: a cache flush or eviction re-opens the window.
    */
    'dedupe' => [
        'store' => env('TALKSASA_PAY_DEDUPE_STORE'), // null = default cache store
        'ttl' => (int) env('TALKSASA_PAY_DEDUPE_TTL', 60 * 60 * 24 * 7), // webhooks retry for ~3 days
        'prefix' => 'talksasa-pay:event:',
    ],

    /*
    | talksasa-pay:poll-events (the reconcile-by-feed backstop to webhooks).
    | cursor_store: "database" (recommended; run the published migration)
    | or "cache" (only with a persistent store: losing the cursor replays the
    | whole feed through the de-duplication above).
    */
    'poller' => [
        'cursor_store' => env('TALKSASA_PAY_CURSOR_STORE', 'cache'),
        'cache_store' => env('TALKSASA_PAY_CURSOR_CACHE_STORE'), // null = default cache store
        'cache_key' => 'talksasa-pay:events-cursor',
        'table' => 'talksasa_pay_cursors',
        'connection' => null, // null = default database connection
        'name' => 'events', // row key, if you poll for several API keys
        'page_size' => 100,
    ],

];
