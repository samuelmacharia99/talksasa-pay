package integrationtest

// mpesaInvariants: what must hold after any M-Pesa chaos scenario.
//
//   - checkInvariants (audit chain, feed gap-free, one webhook job and
//     delivery row per event, event subjects belong to their app, every
//     succeeded/reversed charge paid by a succeeded payment of its app and
//     charge for exactly its amount, each receipt once);
//   - no succeeded M-Pesa payment lacks a receipt;
//   - every app-routed succeeded payment has exactly one charge.succeeded
//     (the payment that paid its charge) or payment.received event naming
//     it; unmatched payments have none; at most one charge.succeeded per
//     charge, naming its paying transaction;
//   - push, transaction and charge states agree (initiating/sent <->
//     pending, unknown <-> needs_review, failed <-> failed, succeeded <->
//     succeeded, or failed + a rescue transaction when a receipt arrived
//     after we gave up); a needs_review charge has a payment in review and
//     a pending one has none; every pending/needs_review STK transaction
//     belongs to a push; no push succeeded twice over;
//   - every stored callback is processed and has its job;
//   - events once per state change for pushes.

func (m *mpesaEnv) mpesaInvariants() {
	m.t.Helper()
	m.checkInvariants()

	m.zero("receipt recorded twice", `
		SELECT count(*) FROM (SELECT 1 FROM transactions WHERE provider_transaction_id IS NOT NULL
		                       GROUP BY provider, provider_transaction_id HAVING count(*) > 1) x`)
	m.zero("succeeded M-Pesa payment without a receipt", `
		SELECT count(*) FROM transactions WHERE status = 'succeeded' AND provider = 'mpesa' AND kind = 'payment'
		   AND provider_transaction_id IS NULL`)
	m.zero("charge paid by a transaction although not succeeded/reversed", `
		SELECT count(*) FROM charges WHERE status NOT IN ('succeeded', 'reversed') AND transaction_id IS NOT NULL`)

	// Events naming money.
	m.zero("app-routed succeeded payment without exactly one charge.succeeded or payment.received event", `
		SELECT count(*) FROM transactions t
		 WHERE t.kind = 'payment' AND t.status = 'succeeded' AND t.app_id IS NOT NULL
		   AND (SELECT count(*) FROM events ev WHERE ev.app_id = t.app_id
		          AND ((ev.type = 'payment.received' AND ev.subject_id = t.id)
		            OR (ev.type = 'charge.succeeded' AND ev.subject_id = t.charge_id
		                AND ev.data->>'transaction_id' = t.id::text))) <> 1`)
	m.zero("charge with more than one charge.succeeded event", `
		SELECT count(*) FROM (SELECT 1 FROM events WHERE type = 'charge.succeeded' GROUP BY subject_id HAVING count(*) > 1) x`)
	m.zero("charge.succeeded naming a transaction that did not pay the charge", `
		SELECT count(*) FROM events ev JOIN charges c ON c.id = ev.subject_id
		 WHERE ev.type = 'charge.succeeded' AND c.transaction_id::text IS DISTINCT FROM ev.data->>'transaction_id'`)
	m.zero("succeeded charge without a charge.succeeded event", `
		SELECT count(*) FROM charges c WHERE c.status IN ('succeeded', 'reversed')
		   AND NOT EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = c.id AND ev.type = 'charge.succeeded')`)
	m.zero("duplicate_of_charge naming a charge the payment did not pay", `
		SELECT count(*) FROM events ev JOIN transactions t ON t.id = ev.subject_id
		 WHERE ev.type = 'payment.received' AND ev.data->>'duplicate_of_charge' IS NOT NULL
		   AND (t.charge_id::text IS DISTINCT FROM ev.data->>'duplicate_of_charge'
		        OR EXISTS (SELECT 1 FROM charges c WHERE c.transaction_id = t.id))`)
	m.zero("unmatched payment with an event", `
		SELECT count(*) FROM transactions t WHERE t.app_id IS NULL
		   AND EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = t.id)`)

	// Push / transaction / charge states agree.
	// rescue(p): the succeeded transaction a late receipt created for a push
	// we had failed (successAfterGiveUp); the push points at it.
	const rescue = `EXISTS (SELECT 1 FROM transactions r WHERE r.id = p.paid_transaction_id AND r.status = 'succeeded'
	                         AND r.channel = 'mpesa_stk' AND r.charge_id = p.charge_id AND r.amount = p.amount)`
	m.zero("push state inconsistent with its transaction", `
		SELECT count(*) FROM stk_pushes p JOIN transactions t ON t.id = p.transaction_id
		 WHERE NOT ((p.status IN ('initiating', 'sent') AND t.status = 'pending')
		         OR (p.status = 'unknown' AND t.status = 'needs_review')
		         OR (p.status = 'failed' AND t.status = 'failed')
		         OR (p.status = 'succeeded' AND t.status = 'succeeded' AND p.paid_transaction_id IS NULL)
		         OR (p.status = 'succeeded' AND t.status = 'failed' AND `+rescue+`))`)
	m.zero("succeeded push with a receipt on its own row AND a rescue row (money twice)", `
		SELECT count(*) FROM stk_pushes p JOIN transactions t ON t.id = p.transaction_id
		 WHERE p.status = 'succeeded' AND t.status = 'succeeded' AND EXISTS (
		       SELECT 1 FROM transactions r WHERE r.id <> p.transaction_id AND r.status = 'succeeded'
		          AND r.channel = 'mpesa_stk' AND r.provider_request_id = p.checkout_request_id)`)
	m.zero("pending/needs_review STK transaction without a push", `
		SELECT count(*) FROM transactions t WHERE t.channel = 'mpesa_stk' AND t.status IN ('pending', 'needs_review')
		   AND NOT EXISTS (SELECT 1 FROM stk_pushes p WHERE p.transaction_id = t.id)`)
	m.zero("STK money for a charge that is neither a push's own transaction, a rescue, nor a second payment", `
		SELECT count(*) FROM transactions t WHERE t.channel = 'mpesa_stk' AND t.status = 'succeeded' AND t.charge_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM stk_pushes p WHERE p.transaction_id = t.id OR p.paid_transaction_id = t.id)
		   AND NOT EXISTS (SELECT 1 FROM stk_pushes p JOIN transactions f ON f.id = p.transaction_id
		                    WHERE p.status = 'succeeded' AND f.status = 'succeeded' AND f.raw_payload ? 'TransID'
		                      AND p.charge_id = t.charge_id AND p.amount = t.amount)`)
	m.zero("needs_review charge without a payment in review", `
		SELECT count(*) FROM charges c WHERE c.status = 'needs_review'
		   AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.charge_id = c.id AND t.status = 'needs_review')`)
	m.zero("pending charge with a payment in review", `
		SELECT count(*) FROM charges c WHERE c.status = 'pending'
		   AND EXISTS (SELECT 1 FROM transactions t WHERE t.charge_id = c.id AND t.status = 'needs_review')`)
	m.zero("live push for a cancelled or reviewed charge", `
		SELECT count(*) FROM stk_pushes p JOIN charges c ON c.id = p.charge_id
		 WHERE p.status IN ('initiating', 'sent') AND c.status IN ('cancelled', 'needs_review')`)
	m.zero("more than one live push per charge", `
		SELECT count(*) FROM (SELECT 1 FROM stk_pushes WHERE status IN ('initiating', 'sent') GROUP BY charge_id HAVING count(*) > 1) x`)

	// Callbacks.
	m.zero("unprocessed callbacks", `SELECT count(*) FROM mpesa_callbacks WHERE processed_at IS NULL`)
	m.zero("stored callbacks without a processing job", `
		SELECT count(*) FROM mpesa_callbacks c WHERE NOT EXISTS (
		  SELECT 1 FROM river_job j WHERE j.kind = 'mpesa_process_callback' AND j.args->>'callback_id' = c.id::text)`)

	// Push events: once per state change.
	m.zero("push with a stk_push.* event type emitted more than once", `
		SELECT count(*) FROM (SELECT 1 FROM events WHERE subject_type = 'stk_push' GROUP BY subject_id, type HAVING count(*) > 1) x`)
	m.zero("push whose current state has no event", `
		SELECT count(*) FROM stk_pushes p WHERE p.status <> 'initiating' AND NOT EXISTS (
		  SELECT 1 FROM events ev WHERE ev.subject_id = p.id AND ev.type = 'stk_push.' ||
		         CASE p.status WHEN 'unknown' THEN 'needs_review' ELSE p.status::text END)`)
	m.zero("succeeded event for a push that is not succeeded", `
		SELECT count(*) FROM stk_pushes p WHERE p.status <> 'succeeded'
		   AND EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = p.id AND ev.type = 'stk_push.succeeded')`)
	m.zero("sent event for a push Daraja never confirmed", `
		SELECT count(*) FROM stk_pushes p WHERE p.checkout_request_id IS NULL
		   AND EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = p.id AND ev.type = 'stk_push.sent')`)
}
