package auditlog

import "testing"

func TestConfigValidation(t *testing.T) {
	valid := DefaultConfig()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []Config{valid, valid, valid, valid}
	cases[0].Overflow = "silent"
	cases[1].QueueSize = -1
	cases[2].RequestIDHeader = "bad header"
	cases[3].SkipPaths = []string{"relative", "[bad"}
	for i, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
