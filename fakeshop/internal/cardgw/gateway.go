// Package cardgw is a make-believe card payment gateway (cardgw.test).
//
// Merchants open a session for an order, send the buyer to /pay/{session},
// and get the buyer back on their return URL with an HMAC-signed result.
// Everything runs in one process, so the "merchant API" is a Go method call.
package cardgw

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/money"
)

// Test cards (the same numbers Stripe uses in test mode).
const (
	CardSuccess = "4242424242424242"
	CardDecline = "4000000000000002"
)

const (
	ChargeSucceeded = "succeeded"
	ChargeDeclined  = "declined"
)

type Session struct {
	ID        string
	Merchant  string
	OrderID   string
	Amount    money.Money
	ReturnURL string
	ChargeID  string // set once a charge succeeded
}

type Charge struct {
	ID        string     `json:"id"`
	SessionID string     `json:"session"`
	Merchant  string     `json:"merchant"`
	OrderID   string     `json:"order_id"`
	Amount    money.JSON `json:"amount"`
	Status    string     `json:"status"`
	Reason    string     `json:"decline_reason,omitempty"`
	Last4     string     `json:"card_last4"`
	CreatedAt int64      `json:"created_at"`
}

// Card is what the buyer typed on the payment page.
type Card struct {
	Number, Exp, CVC, Name string
}

var (
	ErrNoSession   = errors.New("unknown payment session")
	ErrAlreadyPaid = errors.New("session already paid")
)

type Gateway struct {
	secret []byte
	now    func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
	charges  map[string]*Charge
}

func New(secret []byte, now func() time.Time) *Gateway {
	if now == nil {
		now = time.Now
	}
	return &Gateway{secret: secret, now: now, sessions: map[string]*Session{}, charges: map[string]*Charge{}}
}

// CreateSession is the merchant-side call that starts a payment.
func (g *Gateway) CreateSession(merchant, orderID string, amount money.Money, returnURL string) Session {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := &Session{ID: "cs_" + randomHex(12), Merchant: merchant, OrderID: orderID, Amount: amount, ReturnURL: returnURL}
	g.sessions[s.ID] = s
	return *s
}

func (g *Gateway) Session(id string) (Session, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.sessions[id]
	if !ok {
		return Session{}, false
	}
	return *s, true
}

func (g *Gateway) Charge(id string) (Charge, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.charges[id]
	if !ok {
		return Charge{}, false
	}
	return *c, true
}

// Pay charges the card for the session. Input errors (bad expiry etc.)
// return an error without recording anything; a declined card is recorded
// as a declined charge.
func (g *Gateway) Pay(sessionID string, card Card) (Charge, error) {
	number, err := validateCard(card, g.now())
	if err != nil {
		return Charge{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.sessions[sessionID]
	if !ok {
		return Charge{}, ErrNoSession
	}
	if s.ChargeID != "" {
		return Charge{}, ErrAlreadyPaid
	}
	c := &Charge{
		ID: "ch_" + randomHex(12), SessionID: s.ID, Merchant: s.Merchant, OrderID: s.OrderID,
		Amount: s.Amount.JSON(), Last4: number[len(number)-4:], CreatedAt: g.now().Unix(),
	}
	switch number {
	case CardSuccess:
		c.Status = ChargeSucceeded
		s.ChargeID = c.ID
	case CardDecline:
		c.Status, c.Reason = ChargeDeclined, "card_declined"
	default:
		c.Status, c.Reason = ChargeDeclined, "unknown_test_card"
	}
	g.charges[c.ID] = c
	return *c, nil
}

// ReturnURL is where the buyer goes after a successful charge.
func (g *Gateway) ReturnURL(s Session, c Charge) string {
	q := url.Values{}
	q.Set("session", s.ID)
	q.Set("charge", c.ID)
	q.Set("status", c.Status)
	q.Set("sig", g.sign(s.ID, c.ID, c.Status))
	sep := "?"
	if strings.Contains(s.ReturnURL, "?") {
		sep = "&"
	}
	return s.ReturnURL + sep + q.Encode()
}

// VerifyReturn checks the signature a merchant receives on its return URL.
func (g *Gateway) VerifyReturn(sessionID, chargeID, status, sig string) bool {
	want := g.sign(sessionID, chargeID, status)
	return hmac.Equal([]byte(want), []byte(sig))
}

func (g *Gateway) sign(parts ...string) string {
	m := hmac.New(sha256.New, g.secret)
	m.Write([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(m.Sum(nil))
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
