package oauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// userPerKey answers /auth/me with a user of its own for each hlk_u<N> key.
func userPerKey(w http.ResponseWriter, r *http.Request) {
	if n, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer hlk_u"); ok {
		_, _ = w.Write([]byte(`{"id":"user-` + n + `"}`))
		return
	}
	fakeAuthMe(w, r)
}

// newRefreshLimitedProvider is newTestProvider with one user per hlk_u<N> key
// and the per-user refresh limiter on a clock the test moves.
func newRefreshLimitedProvider(t *testing.T) (*httptest.Server, *fakeClock) {
	t.Helper()
	srv, p := newTestProviderUpstream(t, userPerKey)
	clk := &fakeClock{t: time.Now()}
	p.refreshLimiter.now = clk.now
	return srv, clk
}

func TestTokenEndpoint_OnlyFailuresAreLimited(t *testing.T) {
	srv, _ := newRefreshLimitedProvider(t)
	// Well past the 30-request burst from one address, spread over users the
	// way a hosted connector's egress is: those refreshes must not drain it.
	var refresh [4]string
	for u := range refresh {
		_, refresh[u] = connect(t, srv, fmt.Sprintf("hlk_u%d", u))
	}
	for range 10 {
		for u := range refresh {
			_, refresh[u] = refreshGrant(t, srv, refresh[u])
		}
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

func TestTokenEndpoint_RefreshLimitedPerUser(t *testing.T) {
	srv, clk := newRefreshLimitedProvider(t)
	_, looping := connect(t, srv, "hlk_u1")
	_, other := connect(t, srv, "hlk_u2")
	limited := false
	for range 20 {
		status, m := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {looping}})
		if status == http.StatusTooManyRequests {
			limited = true
			break
		}
		if status != http.StatusOK {
			t.Fatalf("refresh failed: %d %v", status, m)
		}
		looping, _ = m["refresh_token"].(string)
	}
	if !limited {
		t.Fatal("one user's refresh loop must be cut off")
	}
	// More refused refreshes than the per-address burst: none may be charged
	// to the address, or the other user behind it would be refused too.
	for range 40 {
		if status, _ := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {looping}}); status != http.StatusTooManyRequests {
			t.Fatalf("a user over the refresh limit got %d, want 429", status)
		}
	}
	_, _ = refreshGrant(t, srv, other)
	// A refused refresh leaves the grant as it was.
	clk.advance(time.Minute)
	_, _ = refreshGrant(t, srv, looping)
}
