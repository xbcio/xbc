package apikey

import "testing"

func TestConfigRejectsUnsafeSettings(t *testing.T) {
	cases := []Config{
		{Header: "Bad Header", AppIDHeader: defaultAppIDHeader, AllowBearer: true, BearerScheme: defaultScheme, MinKeyBytes: 32},
		{Header: "Authorization", AppIDHeader: defaultAppIDHeader, AllowBearer: true, BearerScheme: defaultScheme, MinKeyBytes: 32},
		{Header: defaultHeader, AppIDHeader: defaultAppIDHeader, AllowBearer: true, BearerScheme: "Bad Scheme", MinKeyBytes: 32},
		{Header: defaultHeader, AppIDHeader: defaultAppIDHeader, AllowBearer: true, BearerScheme: defaultScheme, MinKeyBytes: 8},
	}
	for i, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("case %d unexpectedly valid", i)
		}
	}
}

func TestStaticRepositoryValidation(t *testing.T) {
	if _, err := NewStaticRepository(nil); err == nil {
		t.Fatal("empty repository accepted")
	}
	if _, err := NewStaticRepository([]StaticCredential{{Subject: "s", SHA256: "short"}}); err == nil {
		t.Fatal("short key accepted")
	}
	key := "0123456789abcdef0123456789abcdef"
	if _, err := NewStaticRepository([]StaticCredential{{ID: "a", Subject: "a", SHA256: HashKey(key).String()}, {ID: "b", Subject: "b", SHA256: HashKey(key).String()}}); err == nil {
		t.Fatal("duplicate key accepted")
	}
}
