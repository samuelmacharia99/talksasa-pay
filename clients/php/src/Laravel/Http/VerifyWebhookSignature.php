<?php

declare(strict_types=1);

namespace Talksasa\Pay\Laravel\Http;

use Closure;
use Illuminate\Contracts\Config\Repository as Config;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Psr\Log\LoggerInterface;
use Symfony\Component\HttpFoundation\Response;
use Talksasa\Pay\Webhook\Verifier;
use Talksasa\Pay\Webhook\WebhookException;

/**
 * Rejects any request whose Talksasa-Signature does not verify (400) and
 * puts the decoded Event on the request as attribute "talksasa_pay_event".
 */
final class VerifyWebhookSignature
{
    public const ATTRIBUTE = 'talksasa_pay_event';

    public function __construct(
        private readonly Config $config,
        private readonly LoggerInterface $logger,
    ) {
    }

    public function handle(Request $request, Closure $next): Response
    {
        try {
            $event = Verifier::verify(
                $request->getContent(),
                (string) $request->headers->get(Verifier::HEADER, ''),
                (array) $this->config->get('talksasa-pay.webhook_secrets', []),
                null,
                (int) $this->config->get('talksasa-pay.webhook_tolerance', Verifier::DEFAULT_TOLERANCE),
            );
        } catch (WebhookException $e) {
            $this->logger->warning('Talksasa Pay webhook rejected', [
                'reason' => $e->reason,
                'ip' => $request->ip(),
                'event_id' => $request->headers->get('Talksasa-Event-Id'),
            ]);
            return new JsonResponse(['error' => $e->reason], 400);
        }

        $request->attributes->set(self::ATTRIBUTE, $event);

        return $next($request);
    }
}
