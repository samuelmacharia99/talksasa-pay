package main

import (
	"strings"
	"testing"
)

func TestRefusesOutsideDevelopment(t *testing.T) {
	for _, env := range []string{"production", "staging", "test"} {
		t.Setenv("TSP_ENV", env)
		if err := run(nil); err == nil || !strings.Contains(err.Error(), "development only") {
			t.Errorf("%s: %v", env, err)
		}
	}
}

func TestRejectsBadDelay(t *testing.T) {
	t.Setenv("TSP_ENV", "development")
	t.Setenv("TSP_FAKE_DARAJA_CALLBACK_DELAY", "forever")
	if err := run(nil); err == nil {
		t.Fatal("want error")
	}
}
