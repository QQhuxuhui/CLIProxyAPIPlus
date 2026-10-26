package auth

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func testJWT(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return "e30." + payload + ".sig"
}

func TestParseJWTExpCachesPerToken(t *testing.T) {
	first, second := testJWT(1900000000), testJWT(1900003600)
	for i := 0; i < 2; i++ {
		exp, ok := parseJWTExp(first)
		if !ok || !exp.Equal(time.Unix(1900000000, 0)) {
			t.Fatalf("first token exp = %v %v", exp, ok)
		}
	}
	// A refreshed token is a different string and must not reuse the old expiry.
	if exp, ok := parseJWTExp(second); !ok || !exp.Equal(time.Unix(1900003600, 0)) {
		t.Fatalf("second token exp = %v %v", exp, ok)
	}
	if _, ok := parseJWTExp("not-a-jwt"); ok {
		t.Fatal("invalid token parsed")
	}
	if _, ok := parseJWTExp("not-a-jwt"); ok {
		t.Fatal("cached invalid token parsed")
	}
}
