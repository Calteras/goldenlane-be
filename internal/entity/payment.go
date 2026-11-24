package entity

import "time"

// Payment providers. Each merchant brings its own account with one of them.
const (
	ProviderXendit   = "XENDIT"
	ProviderMidtrans = "MIDTRANS"
)

// Payment attempt statuses.
const (
	PaymentPending   = "PENDING"
	PaymentPaid      = "PAID"
	PaymentFailed    = "FAILED"
	PaymentExpired   = "EXPIRED"
	PaymentCancelled = "CANCELLED"
)

// Refund statuses of a paid payment. A payment with no refund status has not needed one.
const (
	RefundPending = "PENDING"
	RefundDone    = "DONE"
	RefundFailed  = "FAILED" // the gateway refused; the merchant must refund by hand
)

// Payment methods as the POS names them.
const (
	MethodQRIS         = "QRIS"
	MethodEWallet      = "E_WALLET"
	MethodBankTransfer = "BANK_TRANSFER"
	MethodOther        = "OTHER"
)

// Ledger entry kinds.
const (
	FeeCustomer = "CUSTOMER_FEE"
	FeeMerchant = "MERCHANT_FEE"
)

// Why an order was not paid.
const (
	ErrorPaymentExpired = "PAYMENT_EXPIRED"
)

// Gateway is a merchant's own payment account.
type Gateway struct {
	ID             string     `json:"id"`
	MerchantID     string     `json:"merchantId"`
	Provider       string     `json:"provider"`
	Sandbox        bool       `json:"sandbox"`
	Active         bool       `json:"active"`
	VerifiedAt     *time.Time `json:"verifiedAt"`
	CredentialsEnc string     `json:"-"` // sealed; never leaves the service
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// Payment is one attempt to pay an order.
type Payment struct {
	ID               string
	OrderID          string
	GatewayID        string
	Provider         string
	Attempt          int
	ExternalID       string
	ProviderChargeID *string
	Status           string
	AmountMinor      int64
	OrderTotalMinor  int64
	CustomerFeeMinor int64
	MerchantFeeMinor int64
	CustomerFeeBps   int
	MerchantFeeBps   int
	PayURL           *string
	ExpiresAt        *time.Time
	PaidAt           *time.Time
	Method           *string
	ErrorCode        *string
	ErrorMessage     *string
	RefundStatus     *string
	RefundAttempts   int
	RefundedAt       *time.Time
	CreatedAt        time.Time
}

// FeeRates are the platform's fees as basis points (1/100 of a percent) of the order total.
type FeeRates struct {
	CustomerBps int
	MerchantBps int
}

// Fees is the money of one online order, in minor units.
type Fees struct {
	// ChargeMinor is what the customer pays: the order total plus the customer fee, in whole rupiah.
	ChargeMinor      int64
	CustomerFeeMinor int64
	// MerchantFeeMinor is what the merchant owes the platform on completion.
	MerchantFeeMinor int64
}

const _rupiah = 100 // minor units in one rupiah: gateways take whole rupiah

// Compute works out the fees for an order total. The customer fee is rounded up so the charge
// is a whole number of rupiah; the merchant fee is rounded to the nearest rupiah.
func (r FeeRates) Compute(totalMinor int64) Fees {
	raw := ceilDiv(totalMinor*int64(r.CustomerBps), 10000)
	charge := ceilDiv(totalMinor+raw, _rupiah) * _rupiah

	merchant := (totalMinor*int64(r.MerchantBps)/10000 + _rupiah/2) / _rupiah * _rupiah

	return Fees{ChargeMinor: charge, CustomerFeeMinor: charge - totalMinor, MerchantFeeMinor: merchant}
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// Where a fee came from.
const (
	FeeSourceOnline = "ONLINE"
	FeeSourcePickup = "PICKUP"
)

// FeeEntry is one line of what a merchant owes.
type FeeEntry struct {
	OrderID     string     `json:"orderId"`
	PaymentID   *string    `json:"paymentId"`
	Kind        string     `json:"kind"`
	Source      string     `json:"source"`
	AmountMinor int64      `json:"amountMinor"`
	CreatedAt   time.Time  `json:"createdAt"`
	SettledAt   *time.Time `json:"settledAt"`
}

// FeeLine is a group of fees as it is shown on an invoice.
type FeeLine struct {
	Code        string `json:"code"` // ONLINE_ORDERS or PICKUP_ORDERS
	Label       string `json:"label"`
	Orders      int    `json:"orders"`
	AmountMinor int64  `json:"amountMinor"`
}

// FeeReservation is the fees set aside for one billing period, or what is owed now.
type FeeReservation struct {
	BillingRef string    `json:"billingRef,omitempty"`
	Lines      []FeeLine `json:"lines"`
	TotalMinor int64     `json:"totalMinor"`
	// Collected means this period's fees were already collected: nothing more is owed for it.
	Collected bool `json:"collected,omitempty"`
}

// FeeStatement totals the ledger of one merchant over a period.
type FeeStatement struct {
	MerchantID       string    `json:"merchantId"`
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	Orders           int       `json:"orders"`
	SalesMinor       int64     `json:"salesMinor"`
	CustomerFeeMinor int64     `json:"customerFeeMinor"`
	MerchantFeeMinor int64     `json:"merchantFeeMinor"`
	TotalOwedMinor   int64     `json:"totalOwedMinor"`
	// SettledMinor is the part of TotalOwedMinor already collected.
	SettledMinor int64 `json:"settledMinor"`
	// OutstandingMinor is what the merchant owes now, from any period.
	OutstandingMinor int64      `json:"outstandingMinor"`
	Entries          []FeeEntry `json:"entries"`
}
