package billing

import "context"

// PaymentProvider is the extension point for payment methods. MVP methods (cash,
// card, static-QR-with-manual-confirmation) all resolve immediately to CONFIRMED;
// a future Fonepay dynamic-QR provider will implement async confirmation here
// (create QR -> poll/websocket status -> confirm idempotently) without changing
// the billing flow or the API.
type PaymentProvider interface {
	Method() string
	// Record validates and (for MVP) confirms a payment. Returns the status to
	// store ("CONFIRMED") and the reference to keep for reconciliation.
	Record(ctx context.Context, amountPaisa int64, reference string) (status, ref string, err error)
}

// manualProvider records a payment as immediately confirmed — the cashier has
// seen cash / the card terminal / the QR app confirmation.
type manualProvider struct{ method string }

func (p manualProvider) Method() string { return p.method }

func (p manualProvider) Record(_ context.Context, _ int64, reference string) (string, string, error) {
	return "CONFIRMED", reference, nil
}

func defaultProviders() map[string]PaymentProvider {
	m := map[string]PaymentProvider{}
	for _, method := range []string{"CASH", "CARD", "QR_FONEPAY", "QR_OTHER"} {
		m[method] = manualProvider{method: method}
	}
	return m
}
