// Package catalog describes the fictional shops of the lab (docs/lab.md "店の商品").
package catalog

import "github.com/pad01g/proxy-shopping-go/fakeshop/internal/money"

// Payment is how a shop takes money.
type Payment string

const (
	PaymentCard     Payment = "card"      // online, through a payment gateway
	PaymentCashOnly Payment = "cash-only" // at the counter only
)

type Product struct {
	SKU   string
	Name  string
	Price money.Money
}

type Shop struct {
	Host     string
	Name     string
	Lang     string // "ja" or "en"; selects UI text
	Currency string
	Shipping money.Money
	Payment  Payment
	// Gateway is the host of the payment page, advertised through
	// <meta name="ps-payment-gateway">. Empty for cash-only shops.
	Gateway string
	// SelfHostedPay marks shops that collect card data on their own
	// /cheap-pay page instead of redirecting to cardgw.test.
	SelfHostedPay bool
	Region        string // spec §2.5 region code
	OrderPrefix   string
	Carrier       string
	Products      []Product
}

func (s *Shop) Product(sku string) (Product, bool) {
	for _, p := range s.Products {
		if p.SKU == sku {
			return p, true
		}
	}
	return Product{}, false
}

func jpy(n int64) money.Money     { return money.New(n, "JPY") }
func usd(cents int64) money.Money { return money.New(cents, "USD") }

// Hosts of the non-shop services served by the same binary.
const (
	GatewayHost = "cardgw.test"
	RatesHost   = "rates.test"
)

// Shops returns a fresh copy of the lab catalog.
func Shops() []*Shop {
	return []*Shop{
		{
			Host: "safe-shop.test", Name: "安心ショップ", Lang: "ja", Currency: "JPY",
			Shipping: jpy(800), Payment: PaymentCard, Gateway: GatewayHost,
			Region: "JP-13", OrderPrefix: "SS", Carrier: "Yamato",
			Products: []Product{
				{"A-100", "抹茶ティーセット", jpy(3200)},
				{"A-200", "南部鉄器の急須", jpy(12000)},
				{"FAIL-100", "配送に失敗する商品", jpy(2000)},
			},
		},
		{
			Host: "us-shop.test", Name: "US Shop", Lang: "en", Currency: "USD",
			Shipping: usd(1000), Payment: PaymentCard, Gateway: GatewayHost,
			Region: "US", OrderPrefix: "US", Carrier: "UPS",
			Products: []Product{
				{"U-100", "Coffee beans 1kg", usd(2500)},
			},
		},
		{
			Host: "cash-store.test", Name: "新宿和菓子店", Lang: "ja", Currency: "JPY",
			Shipping: jpy(1000), Payment: PaymentCashOnly,
			Region: "JP-13-13104", OrderPrefix: "CS", Carrier: "Yamato",
			Products: []Product{
				{"C-100", "店頭限定の和菓子", jpy(1500)},
			},
		},
		{
			Host: "risky-shop.test", Name: "Risky Deals", Lang: "en", Currency: "USD",
			Shipping: usd(0), Payment: PaymentCard, Gateway: "risky-shop.test", SelfHostedPay: true,
			Region: "US", OrderPrefix: "RK", Carrier: "Unknown Post",
			Products: []Product{
				{"R-100", "Too-cheap headphones", usd(999)},
			},
		},
	}
}
