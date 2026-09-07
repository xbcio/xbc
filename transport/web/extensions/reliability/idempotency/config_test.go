package idempotency

import "testing"

func TestConfigValidation(t *testing.T) {
	valid := DefaultConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []Config{valid, valid, valid, valid, valid}
	cases[0].Backend = "database"
	cases[1].PendingStatus = 200
	cases[2].MinKeyLength, cases[2].MaxKeyLength = 100, 20
	cases[3].RedisPrefix = "bad\nkey"
	cases[3].Backend = BackendRedis
	cases[4].RedisInstance = "bad instance!"
	for i, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestPrepareConfigValidatesWithoutMutatingValidInput(t *testing.T) {
	valid := DefaultConfig()
	prepared, err := prepareConfig(valid)
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	if prepared != valid {
		t.Fatalf("prepareConfig() = %#v, want %#v", prepared, valid)
	}

	invalid := valid
	invalid.Backend = "database"
	if _, err := prepareConfig(invalid); err == nil {
		t.Fatal("prepareConfig() accepted an invalid backend")
	}
}
