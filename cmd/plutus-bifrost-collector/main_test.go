package main

import (
	"strings"
	"testing"
)

func TestRedactHidesPasswords(t *testing.T) {
	const pw = "pw-" + "SECRET" // fragments: no literal credential-in-URL in source

	if got := redact("http://u:" + pw + "@bifrost:8080/x"); strings.Contains(got, pw) {
		t.Errorf("password leaked: %s", got)
	}
	if got := redact("http://bifrost:8080"); got != "http://bifrost:8080" {
		t.Errorf("plain URL changed: %s", got)
	}
	if got := redact("http://u:" + pw + "@ho st"); strings.Contains(got, pw) {
		t.Errorf("unparseable URL leaked: %s", got)
	}
}
