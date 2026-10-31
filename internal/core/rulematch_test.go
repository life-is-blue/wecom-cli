package core

import "testing"

func TestRuleMatchUnderscoreIsLiteral(t *testing.T) {
	tests := []struct {
		p, k string
		want bool
	}{
		{"stage.dual_run", "stage.dual_run", true},
		{"stage.dual_run", "stage.dualXrun", false}, // the seeded-pattern wildcard bug
		{`stage.dual\_run`, "stage.dual_run", true},
		{`stage.dual\_run`, "stage.dualXrun", false},
		{"stage.%", "stage.dual_run", true},
		{"stage.%", "stageXdual_run", false},
		{"a.b", "aXb", false},
		{`100\%`, "100%", true},
		{`100\%`, "100x", false},
	}
	for _, tc := range tests {
		if got := RuleMatch(tc.p, tc.k); got != tc.want {
			t.Errorf("RuleMatch(%q,%q)=%v want %v", tc.p, tc.k, got, tc.want)
		}
	}
}

// A rule seeded for stage.dual_run must not govern a look-alike key.
func TestCheckRulesUnderscoreNotWildcard(t *testing.T) {
	cfg := Config{Rules: []Rule{{"stage.dual_run", sp("pass"), "delivery", nil}}}
	in := Input{Cfg: cfg, Actor: Actor{Account: "x", Side: SideCustomer, Role: RoleEditor, Channel: ChannelAPI}}
	// customer may not set stage.dual_run, but stage.dualXrun is not covered by any rule
	if err := checkRules(in, Change{Key: "stage.dual_run", New: JSON("pass")}, nil); err == nil {
		t.Error("customer must be denied on stage.dual_run")
	}
	if err := checkRules(in, Change{Key: "stage.dualXrun", New: JSON("pass")}, nil); err != nil {
		t.Errorf("look-alike key must be unconstrained: %v", err)
	}
}
