package operator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

func TestReports(t *testing.T) {
	relays := []string{testutil.StartRelay(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mk := func(name string) (*messenger.Messenger, *store.DB) {
		db, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		pool := nostrnet.NewPool(nil, testutil.Logger(t))
		t.Cleanup(pool.Close)
		m, err := messenger.New(messenger.Config{Secret: nostr.GeneratePrivateKey(), Pool: pool, DB: db, Inbox: relays, Log: testutil.Logger(t)})
		if err != nil {
			t.Fatal(err)
		}
		return m, db
	}
	opM, opDB := mk("operator")
	in := New(opM, opDB, testutil.Logger(t))
	opM.Start(ctx)
	reporter, _ := mk("reporter")
	reporter.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	sk := nostr.GeneratePrivateKey()
	quoted, _ := giftwrap.NewInner(sk, opM.Pub, "000102030405060708090a0b0c0d0e0f", proto.TypeDisputeRuling, map[string]string{}, nostr.Now())
	forged := *quoted
	forged.Content = `{"x":1}`
	if _, err := reporter.Send(ctx, opM.Pub, "000102030405060708090a0b0c0d0e0f", proto.TypeReport,
		proto.Report{Subject: "abc", OrderID: "000102030405060708090a0b0c0d0e0f", Text: "unfair ruling", Evidence: []*nostr.Event{quoted, &forged}}, nil); err != nil {
		t.Fatal(err)
	}
	for {
		reps, err := in.Reports()
		if err != nil {
			t.Fatal(err)
		}
		if len(reps) == 1 {
			if reps[0].From != reporter.Pub || reps[0].Report.Text != "unfair ruling" || reps[0].InvalidEvidence != 1 {
				t.Fatalf("%+v", reps[0])
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("report not stored")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
