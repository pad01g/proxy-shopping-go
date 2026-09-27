// Command fakeshop serves the lab's fictional shops, the card gateway and
// the rate mock on one port, routed by Host header (docs/lab.md).
package main

import (
	"crypto/rand"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/app"
)

func main() {
	listen := flag.String("listen", envOr("FAKESHOP_LISTEN", ":8080"), "listen address")
	token := flag.String("admin-token", envOr("FAKESHOP_ADMIN_TOKEN", "lab"), "X-Admin-Token for /admin endpoints")
	scheme := flag.String("scheme", os.Getenv("FAKESHOP_SCHEME"), "force http/https in absolute URLs (default: from X-Forwarded-Proto)")
	flag.Parse()

	secret := []byte(os.Getenv("FAKESHOP_GATEWAY_SECRET"))
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Fatal(err)
		}
	}

	a := app.New(app.Options{AdminToken: *token, GatewaySecret: secret, Scheme: *scheme})
	srv := &http.Server{Addr: *listen, Handler: logRequests(a), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fakeshop listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s%s %s", r.Method, r.Host, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
	})
}
