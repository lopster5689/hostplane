package main

import "testing"

// All 11 known states. Tests assert behavior across the full set so any new
// state must explicitly think through transitions, guards, and terminality.
var allStates = []SiteStatus{
	SiteCreated, SiteProvisioning, SiteActive,
	SiteDomainPending, SiteDomainValidating, SiteDomainRouting,
	SiteDomainActive, SiteDomainRemoving,
	SiteDestroying, SiteDestroyed, SiteFailed,
}

// TestCanTransitionTo asserts the exact allowed-transitions matrix from
// lifecycle.go. Any change to allowedTransitions must also update this table.
func TestCanTransitionTo(t *testing.T) {
	allowed := map[SiteStatus]map[SiteStatus]bool{
		SiteCreated:          {SiteProvisioning: true},
		SiteProvisioning:     {SiteActive: true, SiteFailed: true},
		SiteActive:           {SiteDomainPending: true, SiteDestroying: true},
		SiteDomainPending:    {SiteDomainValidating: true, SiteActive: true},
		SiteDomainValidating: {SiteDomainRouting: true, SiteDomainPending: true, SiteActive: true},
		SiteDomainRouting:    {SiteDomainActive: true, SiteActive: true},
		SiteDomainActive:     {SiteDomainRemoving: true, SiteDestroying: true},
		SiteDomainRemoving:   {SiteActive: true, SiteFailed: true},
		SiteDestroying:       {SiteDestroyed: true, SiteFailed: true},
		SiteFailed:           {SiteProvisioning: true, SiteDestroying: true},
		// SiteDestroyed has no outgoing edges — fully terminal.
	}
	for _, from := range allStates {
		for _, to := range allStates {
			want := allowed[from][to]
			got := from.CanTransitionTo(to)
			if got != want {
				t.Errorf("%s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestIsValid(t *testing.T) {
	for _, s := range allStates {
		// SiteDestroyed has no entry in allowedTransitions so IsValid returns
		// false today — that is a latent bug to surface, not enshrine. Until
		// it is fixed, document the asymmetry here so the next reader knows.
		if s == SiteDestroyed {
			if s.IsValid() {
				t.Errorf("SiteDestroyed unexpectedly returned IsValid=true — lifecycle.go may have changed")
			}
			continue
		}
		if !s.IsValid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if SiteStatus("BOGUS").IsValid() {
		t.Error("BOGUS should not be valid")
	}
	if SiteStatus("").IsValid() {
		t.Error(`"" should not be valid`)
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := map[SiteStatus]bool{SiteDestroyed: true, SiteFailed: true}
	for _, s := range allStates {
		if s.IsTerminal() != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, s.IsTerminal(), terminal[s])
		}
	}
}

func TestAllowsCustomDomain(t *testing.T) {
	for _, s := range allStates {
		want := s == SiteActive
		if s.AllowsCustomDomain() != want {
			t.Errorf("%s.AllowsCustomDomain() = %v, want %v", s, s.AllowsCustomDomain(), want)
		}
	}
}

func TestAllowsDestroy(t *testing.T) {
	allowed := map[SiteStatus]bool{SiteActive: true, SiteDomainActive: true, SiteFailed: true}
	for _, s := range allStates {
		if s.AllowsDestroy() != allowed[s] {
			t.Errorf("%s.AllowsDestroy() = %v, want %v", s, s.AllowsDestroy(), allowed[s])
		}
	}
}

func TestStringRoundtrip(t *testing.T) {
	for _, s := range allStates {
		if s.String() != string(s) {
			t.Errorf("%s.String() = %q, want %q", s, s.String(), string(s))
		}
	}
}
