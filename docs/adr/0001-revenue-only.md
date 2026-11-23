# ADR 0001: Talksasa Pay collects only Talksasa's own revenue

- Status: accepted
- Date: 2026-11-22

## Context
Talksasa Home of Creatives Ltd runs several products (ISP Billing SaaS, Cloud
Hosting, more later). Each would otherwise need its own M-Pesa paybill.
Collecting money *on behalf of* other businesses (e.g. an ISP tenant's
subscribers) would make Talksasa a payment service provider under the
National Payment System Act, which needs Central Bank of Kenya authorisation.

## Decision
Talksasa Pay is an internal collection layer on one Talksasa paybill. Every
shilling that enters is Talksasa's own revenue from Talksasa's own customers.

The system is built so that this is a structural property, not just a policy:

1. **No third-party settlement.** `products` has no bank, till or payout
   configuration. No table, column or API moves money to a product or a
   product's customer's customer.
2. **Outgoing money is refunds only.** `direction = 'out'` transactions are
   only refunds or reversals to Talksasa's own customers. They are created
   by named admins with maker-checker approval (Phase 4), never by a product
   API key.
3. **ISPs' end-customer payments never touch this system.** ISPs collect from
   their subscribers on their own tills/paybills. No account prefix or API
   represents an ISP's subscribers.
4. **Products are Talksasa's.** A product row represents a Talksasa business
   line, created only by a Talksasa operator via the admin CLI/panel and
   recorded in the audit log.

## Consequences
- Any future feature that would route money to a non-Talksasa party
  (marketplaces, split payments, collecting for tenants) is out of scope and
  needs a new ADR plus legal review first.
- A test asserts that no `/v1` product API route reaches payout code (added
  with the B2C phase).
- Points for the lawyer to confirm are listed in the Phase 7 security review.
