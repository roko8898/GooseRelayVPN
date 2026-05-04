package carrier

import (
	"testing"
	"time"
)

func TestNextQuotaReset_AdvancesAcrossMidnightPacific(t *testing.T) {
	loc := quotaResetTZ
	// 14:00 PT on a fixed day → next reset is the following midnight PT.
	now := time.Date(2026, 5, 4, 14, 0, 0, 0, loc)
	got := nextQuotaReset(now)
	want := time.Date(2026, 5, 5, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("nextQuotaReset(%v) = %v, want %v", now, got, want)
	}

	// Exactly at midnight PT → next reset must move to the *following* day,
	// not return the same instant (otherwise touchDailyWindow would loop
	// reset → bump → over again on the same boundary).
	atBoundary := time.Date(2026, 5, 5, 0, 0, 0, 0, loc)
	gotBoundary := nextQuotaReset(atBoundary)
	wantBoundary := time.Date(2026, 5, 6, 0, 0, 0, 0, loc)
	if !gotBoundary.Equal(wantBoundary) {
		t.Fatalf("nextQuotaReset at boundary = %v, want %v", gotBoundary, wantBoundary)
	}
}

func TestBumpDailyCount_RollsOverAtReset(t *testing.T) {
	c := &Client{endpoints: []relayEndpoint{{url: "u1"}}}

	// First bump initialises the window and increments to 1.
	c.bumpDailyCount(0)
	if got := c.endpoints[0].dailyCount; got != 1 {
		t.Fatalf("after 1 bump dailyCount=%d want 1", got)
	}

	// Force the window to be in the past so the next bump triggers a reset.
	c.endpoints[0].dailyResetAt = time.Now().Add(-time.Minute)
	c.endpoints[0].dailyCount = 42
	c.bumpDailyCount(0)
	if got := c.endpoints[0].dailyCount; got != 1 {
		t.Fatalf("after rollover dailyCount=%d want 1", got)
	}
	if !c.endpoints[0].dailyResetAt.After(time.Now()) {
		t.Fatalf("dailyResetAt should advance to a future instant after rollover")
	}
}

func TestRecordScriptStatsFromBody_ParsesValidJSON(t *testing.T) {
	c := &Client{endpoints: []relayEndpoint{{url: "u1"}}}
	body := []byte(`{"ok":true,"date":"2026-05-04","count":4321}`)
	c.recordScriptStatsFromBody(0, "u1", body)
	if got := c.endpoints[0].scriptCount; got != 4321 {
		t.Fatalf("scriptCount=%d want 4321", got)
	}
	if c.endpoints[0].scriptCountAt.IsZero() {
		t.Fatalf("scriptCountAt should be set after a successful parse")
	}
}

func TestRecordScriptStatsFromBody_LegacyTextResponse(t *testing.T) {
	// Old apps_script/Code.gs returns "GooseRelay forwarder OK" from doGet.
	// We must not panic, must not record a count, and must flag the once-log.
	c := &Client{endpoints: []relayEndpoint{{url: "u1"}}}
	c.recordScriptStatsFromBody(0, "u1", []byte("GooseRelay forwarder OK"))
	if !c.endpoints[0].scriptCountAt.IsZero() {
		t.Fatalf("scriptCountAt should remain zero on a non-JSON body")
	}
	if !c.endpoints[0].scriptStatsErrLogged {
		t.Fatalf("scriptStatsErrLogged should be set so future hours don't re-log")
	}
}

func TestRecordScriptStatsFromBody_RecoveryAfterRedeploy(t *testing.T) {
	// Simulate operator redeploying: first poll returns legacy text (logs once),
	// next poll returns JSON. We should record the count AND clear the
	// once-flag so a future regression would log a fresh warning.
	c := &Client{endpoints: []relayEndpoint{{url: "u1"}}}
	c.recordScriptStatsFromBody(0, "u1", []byte("GooseRelay forwarder OK"))
	if !c.endpoints[0].scriptStatsErrLogged {
		t.Fatalf("first call should set scriptStatsErrLogged")
	}
	c.recordScriptStatsFromBody(0, "u1", []byte(`{"ok":true,"date":"2026-05-04","count":7}`))
	if c.endpoints[0].scriptStatsErrLogged {
		t.Fatalf("scriptStatsErrLogged should clear after a successful parse")
	}
	if c.endpoints[0].scriptCount != 7 {
		t.Fatalf("scriptCount=%d want 7", c.endpoints[0].scriptCount)
	}
}
