package preflight

import (
	"fmt"
	"strings"
	"testing"

	// Import testutil to register the -test.root flag used by containerd tests
	_ "github.com/containerd/containerd/v2/pkg/testutil"
)

func TestKernelVersion(t *testing.T) {
	version, err := KernelVersion()
	if err != nil {
		t.Fatalf("KernelVersion failed: %v", err)
	}

	if version == "" {
		t.Error("expected non-empty kernel version")
	}

	// Version should contain at least major.minor
	if !strings.Contains(version, ".") {
		t.Errorf("expected version to contain '.', got %s", version)
	}

	t.Logf("kernel version: %s", version)
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1, v2   string
		expected int
	}{
		// Equal versions
		{"6.16.0", "6.16.0", 0},
		{"6.16", "6.16.0", 0},
		{"5.17.0", "5.17.0", 0},

		// v1 < v2
		{"5.17.0", "6.16.0", -1},
		{"6.15.0", "6.16.0", -1},
		{"6.16.0", "6.16.1", -1},
		{"5.4.0", "6.0.0", -1},

		// v1 > v2
		{"6.16.0", "5.17.0", 1},
		{"6.16.0", "6.15.0", 1},
		{"6.16.1", "6.16.0", 1},
		{"7.0.0", "6.99.99", 1},

		// With suffixes
		{"6.16.0-rc1", "6.16.0", 0},
		{"6.16.0-generic", "6.16.0", 0},
		{"5.17.0-1-amd64", "5.17.0", 0},
		{"6.16.0-rc1", "6.15.0", 1},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s_vs_%s", tc.v1, tc.v2), func(t *testing.T) {
			result, err := CompareVersions(tc.v1, tc.v2)
			if err != nil {
				t.Fatalf("CompareVersions(%s, %s) failed: %v", tc.v1, tc.v2, err)
			}
			if result != tc.expected {
				t.Errorf("CompareVersions(%s, %s) = %d, want %d", tc.v1, tc.v2, result, tc.expected)
			}
		})
	}
}

func TestCompareVersionsInvalid(t *testing.T) {
	tests := []struct {
		v1, v2 string
	}{
		{"", "6.16.0"},
		{"6.16.0", ""},
		{"abc", "6.16.0"},
		{"6", "6.16.0"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s_vs_%s", tc.v1, tc.v2), func(t *testing.T) {
			_, err := CompareVersions(tc.v1, tc.v2)
			if err == nil {
				t.Errorf("CompareVersions(%s, %s) should have failed", tc.v1, tc.v2)
			}
		})
	}
}

func TestCheckKernelVersion(t *testing.T) {
	// Get current version
	current, err := KernelVersion()
	if err != nil {
		t.Fatalf("KernelVersion failed: %v", err)
	}

	// Should pass for a very old version requirement
	if err := CheckKernelVersion("1.0.0"); err != nil {
		t.Errorf("CheckKernelVersion(1.0.0) should pass on kernel %s: %v", current, err)
	}

	// Should fail for a future version requirement
	if err := CheckKernelVersion("99.0.0"); err == nil {
		t.Error("CheckKernelVersion(99.0.0) should fail")
	}
}

func TestCheckErofsSupport(t *testing.T) {
	err := CheckErofsSupport()
	if err != nil {
		t.Logf("EROFS not available: %v", err)
		t.Skip("EROFS module not loaded")
	}
	t.Log("EROFS is available")
}

func TestCheck(t *testing.T) {
	err := Check()
	if err != nil {
		t.Logf("Preflight check failed: %v", err)
		// Don't fail the test - the system may not meet requirements
		t.Skip("System does not meet preflight requirements")
	}
	t.Log("All preflight checks passed")
}
