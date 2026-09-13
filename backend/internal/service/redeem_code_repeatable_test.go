package service

import "testing"

func TestRepeatableRedeemCodeCanUseUntilMaximum(t *testing.T) {
	code := &RedeemCode{Status: StatusUnused, MaxUses: 3, UsedCount: 2}
	if !code.CanUse() {
		t.Fatal("code should remain usable before max_uses")
	}
	code.UsedCount = 3
	if code.CanUse() {
		t.Fatal("code must stop being usable at max_uses")
	}
}

func TestLegacyRedeemCodeDefaultsToSingleUse(t *testing.T) {
	code := &RedeemCode{Status: StatusUnused}
	if !code.CanUse() {
		t.Fatal("unused legacy code should be usable")
	}
	code.UsedCount = 1
	if code.CanUse() {
		t.Fatal("legacy code without max_uses must remain single-use")
	}
}
