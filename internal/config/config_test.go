package config

import (
	"os"
	"testing"
)

// loadForTest runs Load with only the keys the test sets. It never reads a
// real .env: Load looks in the working directory (this package's folder),
// and the test refuses to run if one were ever placed there.
func loadForTest(t *testing.T, env map[string]string) *Config {
	t.Helper()
	if _, err := os.Stat(".env"); err == nil {
		t.Skip("a .env file sits beside the package; Load would read it")
	}
	t.Setenv("ENV", "dev")
	t.Setenv("MONGO_URI", "mongodb://127.0.0.1:1/config-test")
	t.Setenv("JWT_SECRET", "config-test-secret")
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// REFERRAL_REWARD_PAISE=0 is the founder's documented switch to stop paying
// referrals (handoff 9.6 item 8), and FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE=0
// means no derived member discount. Both used to fall back to the default
// (Rs 100, Rs 2/L) because the accessors treated 0 as unset.
func TestZeroSwitchesOffTheReferralRewardAndTheDerivedDiscount(t *testing.T) {
	off := loadForTest(t, map[string]string{
		"REFERRAL_REWARD_PAISE":               "0",
		"FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE": "0",
	})
	if got := off.ReferralReward(); got != 0 {
		t.Fatalf("REFERRAL_REWARD_PAISE=0 pays Rs %v per side, want 0", got)
	}
	if got := off.FoundingLevel3OffPerLitre(); got != 0 {
		t.Fatalf("FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE=0 takes Rs %v/L off, want 0", got)
	}

	// Unset keys keep the defaults, and so does a Config built in code.
	def := loadForTest(t, map[string]string{"REFERRAL_REWARD_PAISE": "", "FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE": ""})
	if def.ReferralReward() != 100 || def.FoundingLevel3OffPerLitre() != 2 {
		t.Fatalf("defaults: reward %v off %v", def.ReferralReward(), def.FoundingLevel3OffPerLitre())
	}
	if (&Config{}).ReferralReward() != 100 || (&Config{}).FoundingLevel3OffPerLitre() != 2 {
		t.Fatalf("a zero Config must keep the in-code defaults")
	}
	set := loadForTest(t, map[string]string{"REFERRAL_REWARD_PAISE": "7500", "FOUNDING_LEVEL3_OFF_PAISE_PER_LITRE": "300"})
	if set.ReferralReward() != 75 || set.FoundingLevel3OffPerLitre() != 3 {
		t.Fatalf("set values: reward %v off %v", set.ReferralReward(), set.FoundingLevel3OffPerLitre())
	}
}
