package main

import (
	"testing"
	"time"
)

func TestLookupEnvOrInt_InvalidUsesDefault(t *testing.T) {
	t.Setenv("TEST_INT", "notanumber")
	result := LookupEnvOrInt("TEST_INT", 42)
	if result != 42 {
		t.Errorf("expected default 42, got %d", result)
	}
}

func TestLookupEnvOrDuration_InvalidUsesDefault(t *testing.T) {
	t.Setenv("TEST_DUR", "notaduration")
	result := LookupEnvOrDuration("TEST_DUR", time.Minute)
	if result != time.Minute {
		t.Errorf("expected default 1m0s, got %v", result)
	}
}

func TestLookupEnvOrBool_InvalidUsesDefault(t *testing.T) {
	t.Setenv("TEST_BOOL", "notabool")
	result := LookupEnvOrBool("TEST_BOOL", true)
	if !result {
		t.Errorf("expected default true, got %v", result)
	}
}

func TestLookupEnvOrSize_InvalidUsesDefaultAndNonNil(t *testing.T) {
	t.Setenv("TEST_SIZE", "notasize")
	result := LookupEnvOrSize("TEST_SIZE", 1024)
	if result == nil {
		t.Fatal("expected non-nil Size")
	}
	if result.Bytes != 1024 {
		t.Errorf("expected default 1024, got %d", result.Bytes)
	}
}

func TestLookupLogLevel_InvalidUsesInfo(t *testing.T) {
	t.Setenv("ACEXY_LOG_LEVEL", "notalevel")
	result := LookupLogLevel()
	if result != 0 {
		t.Errorf("expected LevelInfo(0), got %v", result)
	}
}
