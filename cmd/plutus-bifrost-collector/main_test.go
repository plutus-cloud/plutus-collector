package main

import (
	"strings"
	"testing"
)

func TestRedactHidesPasswords(t *testing.T) {
	if got := redact("http://u:pw-SECRET@bifrost:8080/x"); strings.Contains(got, "pw-SECRET") {
		t.Errorf("password leaked: %s", got)
	}
	if got := redact("http://bifrost:8080"); got != "http://bifrost:8080" {
		t.Errorf("plain URL changed: %s", got)
	}
	if got := redact("http://u:pw-SECRET@ho st"); strings.Contains(got, "pw-SECRET") {
		t.Errorf("unparseable URL leaked: %s", got)
	}
}
