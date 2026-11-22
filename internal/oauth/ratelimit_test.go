package oauth

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestRateKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":                "203.0.113.9",
		"::ffff:203.0.113.9":         "203.0.113.9",
		"2001:db8:1:2:aaaa::1":       "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::99": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":            "2001:db8:1:3::/64",
		"fe80::1%eth0":               "fe80::/64",
		"not-an-ip":                  "not-an-ip",
	} {
		if got := rateKey(in); got != want {
			t.Errorf("rateKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyedLimiter_FullMapMakesRoom(t *testing.T) {
	l := newKeyedLimiter(60, 1)
	l.max = 2
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("fresh keys must be allowed")
	}
	// Both buckets are active and drained; a third address still gets in.
	if !l.Allow("c") {
		t.Fatal("a full limiter must make room for a new key, not refuse it")
	}
	if len(l.buckets) > l.max {
		t.Fatalf("limiter holds %d keys, cap is %d", len(l.buckets), l.max)
	}
}

func TestKeyedLimiter_PeekAndCharge(t *testing.T) {
	now := time.Now()
	l := newKeyedLimiter(60, 2)
	l.now = func() time.Time { return now }
	for range 5 {
		if !l.Peek("k") {
			t.Fatal("peeking must not spend tokens")
		}
	}
	// Three failures that passed Peek at once: the third is owed.
	l.Charge("k")
	l.Charge("k")
	l.Charge("k")
	if l.Peek("k") {
		t.Fatal("a bucket charged past its burst must be empty")
	}
	now = now.Add(time.Second) // 60/min refills one token a second
	if l.Peek("k") {
		t.Fatal("a bucket in debt must pay it off before it lets a request in")
	}
	now = now.Add(time.Second)
	if !l.Peek("k") {
		t.Fatal("an emptied bucket must refill")
	}
}

func TestTokenEndpoint_OnlyFailuresAreLimited(t *testing.T) {
	srv, _ := newTestProvider(t)
	_, refresh := connect(t, srv, "hlk_k1")
	// Well past the 30-request burst: shared-egress refreshes must not drain it.
	for range 40 {
		_, refresh = refreshGrant(t, srv, refresh)
	}
	limited := false
	for i := range 40 {
		status, _ := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {fmt.Sprintf("junk-%d", i)}})
		if status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("failed token requests must still be rate limited")
	}
}
