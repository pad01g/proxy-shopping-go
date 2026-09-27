package cardgw

import (
	"embed"
	"html/template"
	"log"
	"net/http"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/httpx"
)

//go:embed templates/*.html
var templateFS embed.FS

var payPage = template.Must(template.ParseFS(templateFS, "templates/pay.html"))

type payView struct {
	Session Session
	Card    Card // echoed back on error, never the CVC
	Error   string
	Reason  string
	Charge  string // id of the declined charge, if any
	Paid    bool
}

// Handler serves the cardgw.test host.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pay/{session}", g.showPay)
	mux.HandleFunc("POST /pay/{session}", g.submitPay)
	mux.HandleFunc("GET /api/charges/{id}", g.getCharge)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("cardgw.test – lab payment gateway\n"))
	})
	return mux
}

func (g *Gateway) showPay(w http.ResponseWriter, r *http.Request) {
	s, ok := g.Session(r.PathValue("session"))
	if !ok {
		http.Error(w, ErrNoSession.Error(), http.StatusNotFound)
		return
	}
	render(w, http.StatusOK, payView{Session: s, Paid: s.ChargeID != ""})
}

func (g *Gateway) submitPay(w http.ResponseWriter, r *http.Request) {
	s, ok := g.Session(r.PathValue("session"))
	if !ok {
		http.Error(w, ErrNoSession.Error(), http.StatusNotFound)
		return
	}
	card := Card{Number: r.FormValue("number"), Exp: r.FormValue("exp"), CVC: r.FormValue("cvc"), Name: r.FormValue("name")}
	echo := Card{Number: card.Number, Exp: card.Exp, Name: card.Name}

	c, err := g.Pay(s.ID, card)
	switch {
	case err == ErrAlreadyPaid:
		render(w, http.StatusConflict, payView{Session: s, Paid: true})
	case err != nil:
		render(w, http.StatusUnprocessableEntity, payView{Session: s, Card: echo, Error: err.Error(), Reason: "invalid_input"})
	case c.Status == ChargeDeclined:
		render(w, http.StatusPaymentRequired, payView{Session: s, Card: echo, Error: "Your card was declined.", Reason: c.Reason, Charge: c.ID})
	default:
		http.Redirect(w, r, g.ReturnURL(s, c), http.StatusSeeOther)
	}
}

func (g *Gateway) getCharge(w http.ResponseWriter, r *http.Request) {
	c, ok := g.Charge(r.PathValue("id"))
	if !ok {
		httpx.Error(w, http.StatusNotFound, "charge not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func render(w http.ResponseWriter, status int, v payView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := payPage.Execute(w, v); err != nil {
		log.Printf("cardgw: render: %v", err)
	}
}
