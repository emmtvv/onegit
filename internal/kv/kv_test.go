package kv_test

import (
	"context"
	"testing"
	"time"

	"onegit/internal/kv"
	"onegit/internal/testutil"
)

func TestKV(t *testing.T) {
	cfg := testutil.Config(t)
	k := testutil.KV(t, cfg)
	ctx := context.Background()

	if err := k.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := k.Get(ctx, "missing"); ok || err != nil {
		t.Errorf("Get(missing) = %v, %v", ok, err)
	}
	testutil.Must(t, k.Set(ctx, "a", "1", time.Minute))
	if v, ok, err := k.Get(ctx, "a"); v != "1" || !ok || err != nil {
		t.Errorf("Get(a) = %q, %v, %v", v, ok, err)
	}
	testutil.Must(t, k.Set(ctx, "short", "x", 50*time.Millisecond))
	testutil.Eventually(t, 3*time.Second, "key expiry", func() bool {
		_, ok, _ := k.Get(ctx, "short")
		return !ok
	})
	testutil.Must(t, k.Set(ctx, "b", "2", 0))
	testutil.Must(t, k.Del(ctx, "a", "b"))
	if _, ok, _ := k.Get(ctx, "a"); ok {
		t.Error("Del did not delete")
	}

	type item struct {
		N int
		S []string
	}
	testutil.Must(t, k.SetJSON(ctx, "j", item{N: 3, S: []string{"x"}}, time.Minute))
	var got item
	if ok, err := k.GetJSON(ctx, "j", &got); !ok || err != nil || got.N != 3 || got.S[0] != "x" {
		t.Errorf("GetJSON = %+v, %v, %v", got, ok, err)
	}
	if ok, err := k.GetJSON(ctx, "nope", &got); ok || err != nil {
		t.Errorf("GetJSON(missing) = %v, %v", ok, err)
	}
	testutil.Must(t, k.Set(ctx, "notjson", "{", time.Minute))
	if _, err := k.GetJSON(ctx, "notjson", &got); err == nil {
		t.Error("GetJSON decoded garbage")
	}
	if err := k.SetJSON(ctx, "bad", func() {}, time.Minute); err == nil {
		t.Error("SetJSON accepted an unencodable value")
	}
}

func TestRateLimit(t *testing.T) {
	cfg := testutil.Config(t)
	k := testutil.KV(t, cfg)
	ctx := context.Background()
	if ok, err := k.Allowed(ctx, "login", 3); !ok || err != nil {
		t.Fatalf("fresh counter not allowed: %v", err)
	}
	for i := 1; i <= 4; i++ {
		ok, err := k.Hit(ctx, "login", 3, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if want := i <= 3; ok != want {
			t.Errorf("hit %d: within limit = %v, want %v", i, ok, want)
		}
	}
	if ok, _ := k.Allowed(ctx, "login", 3); ok {
		t.Error("Allowed after exceeding the limit")
	}
	testutil.Must(t, k.ResetHits(ctx, "login"))
	if ok, _ := k.Allowed(ctx, "login", 3); !ok {
		t.Error("ResetHits did not reset")
	}
	// The window expires.
	k.Hit(ctx, "w", 1, 100*time.Millisecond)
	k.Hit(ctx, "w", 1, 100*time.Millisecond)
	testutil.Eventually(t, 3*time.Second, "window expiry", func() bool {
		ok, _ := k.Allowed(ctx, "w", 1)
		return ok
	})
}

func TestPrefixesIsolate(t *testing.T) {
	cfg := testutil.Config(t)
	ctx := context.Background()
	a, err := kv.Open(ctx, cfg.Redis.URL, cfg.Redis.KeyPrefix+"a:")
	testutil.Must(t, err)
	defer a.Close()
	b, err := kv.Open(ctx, cfg.Redis.URL, cfg.Redis.KeyPrefix+"b:")
	testutil.Must(t, err)
	defer b.Close()
	testutil.Must(t, a.Set(ctx, "k", "from a", time.Minute))
	if _, ok, _ := b.Get(ctx, "k"); ok {
		t.Error("prefixes do not isolate keys")
	}
	a.Hit(ctx, "rl", 1, time.Minute)
	a.Hit(ctx, "rl", 1, time.Minute)
	if ok, _ := b.Allowed(ctx, "rl", 1); !ok {
		t.Error("prefixes do not isolate rate limits")
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := kv.Open(ctx, "not-a-url", ""); err == nil {
		t.Error("bad URL accepted")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := kv.Open(ctx, "redis://127.0.0.1:1/0", ""); err == nil {
		t.Error("unreachable redis accepted")
	}
}
