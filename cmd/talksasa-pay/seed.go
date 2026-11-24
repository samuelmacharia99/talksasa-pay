package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// seedDemo fills a DEVELOPMENT database with realistic demo data so the admin
// panel and pay page have something to show. It goes through the normal
// services (so every rule, trigger and audit entry applies) and is safe to
// re-run: payers, requests and receipts are keyed deterministically, so a
// second run changes nothing.
func seedDemo(ctx context.Context, a *app, actor audit.Actor, w io.Writer) error {
	type bill struct {
		ref     string
		desc    string
		amount  int64
		payment int64 // 0 = unpaid
	}
	type customer struct {
		ref, name, phone string
		bills            []bill
	}
	catalogue := []struct {
		name, prefix string
		customers    []customer
	}{
		{"ISP Billing", "BILL", []customer{
			{"isp-1001", "Kilimani Fibre Ltd", "0712000101", []bill{{"INV-24001", "ISP Billing Pro, monthly", 4500, 4500}, {"INV-24017", "SMS bundle 10k", 1500, 0}}},
			{"isp-1002", "Rift Valley Connect", "0722000102", []bill{{"INV-24002", "ISP Billing Starter, monthly", 2500, 1000}}},
			{"isp-1003", "Coastline Wireless", "0733000103", []bill{{"INV-24003", "ISP Billing Pro, annual", 45000, 50000}}},
			{"isp-1004", "Mlimani Networks", "0110000104", []bill{{"INV-24004", "ISP Billing Pro, monthly", 4500, 0}}},
		}},
		{"Cloud Hosting", "HOST", []customer{
			{"host-501", "Amani Bakery", "0745000201", []bill{{"HST-9001", "Shared hosting, 12 months", 6000, 6000}}},
			{"host-502", "Jua Kali Designs", "0757000202", []bill{{"HST-9002", "VPS 4GB, monthly", 3200, 0}, {"HST-9003", ".co.ke domain renewal", 1200, 1200}}},
			{"host-503", "Nairobi Legal LLP", "0768000203", []bill{{"HST-9004", "Business email, 10 seats", 8000, 3000}}},
		}},
	}

	worker := audit.System("seed-demo")
	for _, pc := range catalogue {
		p, err := a.products.Resolve(ctx, pc.prefix)
		if errors.Is(err, product.ErrNotFound) {
			c, err := a.products.Create(ctx, actor, product.CreateParams{Name: pc.name, AccountPrefix: pc.prefix})
			if err != nil {
				return err
			}
			p = c.Product
			fmt.Fprintf(w, "created product %s (%s); its webhook secret was discarded, rotate it to get one\n", p.Name, p.AccountPrefix)
		} else if err != nil {
			return err
		}
		for ci, cu := range pc.customers {
			py, _, err := a.payers.Upsert(ctx, actor, p.ID, payer.UpsertParams{
				ExternalRef: cu.ref, DisplayName: cu.name, Email: cu.ref + "@example.com", Phones: []string{cu.phone},
			})
			if err != nil {
				return err
			}
			for bi, b := range cu.bills {
				c, err := a.paymentRequests.Create(ctx, audit.Product(p.ID), p.ID, paymentrequest.CreateParams{
					PayerAccountID: py.ID, ProductReference: b.ref, Description: b.desc, Amount: b.amount,
					IdempotencyKey: "seed-" + b.ref,
				})
				if err != nil { // a re-run replays the same idempotency key and gets the original back
					return err
				}
				fmt.Fprintf(w, "  %s %-10s %-34s KES %6d  pay page token: %s\n", py.AccountNumber, b.ref, b.desc, b.amount, c.Request.HostedToken)
				if b.payment == 0 {
					continue
				}
				target := c.Request.ID
				receipt := fmt.Sprintf("SD%s%d%d0X", pc.prefix, ci, bi)
				if _, _, err := a.ledger.RecordAndAllocate(ctx, worker, ledger.IncomingPayment{
					Channel: store.TxnChannelMpesaStk, Receipt: receipt, Amount: money.KES(b.payment), Phone: cu.phone,
					BillRef: py.AccountNumber, ProductID: &p.ID, PayerAccountID: &py.ID, PaymentRequestID: &target,
					RawPayload: []byte(fmt.Sprintf(`{"seed":true,"MpesaReceiptNumber":%q,"Amount":%d,"PhoneNumber":%q}`, receipt, b.payment, cu.phone)),
				}); err != nil {
					return err
				}
			}
		}
	}
	// One payment whose account number matches nobody: it lands in suspense.
	if _, _, err := a.ledger.RecordAndAllocate(ctx, worker, ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaC2b, Receipt: "SDSUSPENSE01", Amount: money.KES(2300), Phone: "0799000999",
		BillRef: "HOST-99999", RawPayload: []byte(`{"seed":true,"TransID":"SDSUSPENSE01","BillRefNumber":"HOST-99999"}`),
	}); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nDemo data ready (re-running is safe). Open a pay page at http://127.0.0.1:3000/<token>.")
	return nil
}
