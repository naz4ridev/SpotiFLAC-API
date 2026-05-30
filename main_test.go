package main

import (
	"errors"
	"testing"
)

func TestAbsInt64(t *testing.T) {
	tests := []struct {
		input    int64
		expected int64
	}{
		{5, 5},
		{-5, 5},
		{0, 0},
	}
	for _, tc := range tests {
		res := absInt64(tc.input)
		if res != tc.expected {
			t.Errorf("absInt64(%d) = %d; want %d", tc.input, res, tc.expected)
		}
	}
}

func TestMaxInt64(t *testing.T) {
	tests := []struct {
		a        int64
		b        int64
		expected int64
	}{
		{5, 10, 10},
		{10, 5, 10},
		{-5, -10, -5},
	}
	for _, tc := range tests {
		res := maxInt64(tc.a, tc.b)
		if res != tc.expected {
			t.Errorf("maxInt64(%d, %d) = %d; want %d", tc.a, tc.b, res, tc.expected)
		}
	}
}

func TestValidateMonochromeDuration(t *testing.T) {
	// Backup and defer restore of the package level variable
	origGetAudioDuration := getAudioDuration
	defer func() {
		getAudioDuration = origGetAudioDuration
	}()

	// Case 1: Duration matches within tolerance.
	// Expected: 184000 ms. Tolerance: max(3000 ms, 184000 * 2% = 3680 ms) -> 3680 ms.
	// Actual duration returned: 183500 ms (diff: 500 ms <= 3680 ms).
	getAudioDuration = func(ffprobePath, filePath string) (int64, error) {
		return 183500, nil
	}

	res := validateMonochromeDuration("", "", 184000, true, 3, 2)
	if !res.OK {
		t.Errorf("Expected duration match OK=true, got OK=false, err: %v", res.Err)
	}
	if !res.DurationMatch {
		t.Errorf("Expected duration_match=true, got false")
	}
	if res.ExpectedDurationMs != 184000 || res.ActualDurationMs != 183500 || res.DurationToleranceMs != 3680 {
		t.Errorf("Unexpected metrics: %+v", res)
	}

	// Case 2: Duration mismatches (outside tolerance).
	// Expected: 184000 ms. Actual duration returned: 210000 ms (diff: 26000 ms > 3680 ms).
	getAudioDuration = func(ffprobePath, filePath string) (int64, error) {
		return 210000, nil
	}

	res = validateMonochromeDuration("", "", 184000, true, 3, 2)
	if res.OK {
		t.Errorf("Expected validation failure, but got OK=true")
	}
	if res.DurationMatch {
		t.Errorf("Expected duration_match=false, got true")
	}
	if res.Err == nil || res.Err.Error() != "monochrome duration mismatch: expected 184000 ms, got 210000 ms" {
		t.Errorf("Unexpected error message: %v", res.Err)
	}

	// Case 3: Duration mismatch but validation is not required (requireMatch=false).
	res = validateMonochromeDuration("", "", 184000, false, 3, 2)
	if !res.OK {
		t.Errorf("Expected OK=true because requireMatch=false, got error: %v", res.Err)
	}
	if res.DurationMatch {
		t.Errorf("Expected duration_match=false, got true")
	}

	// Case 4: Missing expected metadata (expectedDurationMs <= 0) and requireMatch=true.
	res = validateMonochromeDuration("", "", 0, true, 3, 2)
	if res.OK {
		t.Errorf("Expected failure when expected duration is missing and requireMatch=true")
	}
	if res.Err == nil || res.Err.Error() != "monochrome duration validation skipped: expected duration unavailable" {
		t.Errorf("Unexpected error message: %v", res.Err)
	}

	// Case 5: Missing expected metadata (expectedDurationMs <= 0) and requireMatch=false.
	res = validateMonochromeDuration("", "", 0, false, 3, 2)
	if !res.OK {
		t.Errorf("Expected OK=true when expected duration is missing but requireMatch=false")
	}

	// Case 6: Actual duration query returns error.
	getAudioDuration = func(ffprobePath, filePath string) (int64, error) {
		return 0, errors.New("ffprobe mock error")
	}

	res = validateMonochromeDuration("", "", 184000, true, 3, 2)
	if res.OK {
		t.Errorf("Expected failure when actual duration retrieval fails")
	}
	if res.Err == nil || res.Err.Error() != "failed to calculate actual duration: ffprobe mock error" {
		t.Errorf("Unexpected error message: %v", res.Err)
	}
}
