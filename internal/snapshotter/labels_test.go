package snapshotter

import (
	"testing"

	"github.com/opencontainers/go-digest"
)

func TestLabelConstants(t *testing.T) {
	// Verify label prefix is consistent
	tests := []struct {
		label string
		name  string
	}{
		{LabelExtract, "extract"},
		{LabelLayerDigest, "layer-digest"},
		{LabelLayerBlobPath, "layer-blob-path"},
		{LabelFsmetaReady, "fsmeta-ready"},
		{LabelLayerOrder, "layer-order"},
		{LabelMountType, "mount-type"},
		{LabelWritableSize, "writable-size"},
		{LabelConversionError, "conversion-error"},
		{LabelFsmetaLayers, "fsmeta-layers"},
		{LabelImmutable, "immutable"},
	}

	for _, tc := range tests {
		expected := LabelPrefix + tc.name
		if tc.label != expected {
			t.Errorf("label %q should be %q, got %q", tc.name, expected, tc.label)
		}
	}
}

func TestEncodeLayerOrder(t *testing.T) {
	tests := []struct {
		name     string
		digests  []digest.Digest
		expected string
	}{
		{
			name:     "empty slice",
			digests:  nil,
			expected: "[]",
		},
		{
			name:     "empty slice explicit",
			digests:  []digest.Digest{},
			expected: "[]",
		},
		{
			name: "single digest",
			digests: []digest.Digest{
				digest.Digest("sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1"),
			},
			expected: `["sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1"]`,
		},
		{
			name: "multiple digests",
			digests: []digest.Digest{
				digest.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111"),
				digest.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
				digest.Digest("sha256:3333333333333333333333333333333333333333333333333333333333333333"),
			},
			expected: `["sha256:1111111111111111111111111111111111111111111111111111111111111111","sha256:2222222222222222222222222222222222222222222222222222222222222222","sha256:3333333333333333333333333333333333333333333333333333333333333333"]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := EncodeLayerOrder(tc.digests)
			if result != tc.expected {
				t.Errorf("EncodeLayerOrder(%v) = %q, want %q", tc.digests, result, tc.expected)
			}
		})
	}
}

func TestDecodeLayerOrder(t *testing.T) {
	tests := []struct {
		name     string
		encoded  string
		expected []digest.Digest
	}{
		{
			name:     "empty string",
			encoded:  "",
			expected: nil,
		},
		{
			name:     "empty array",
			encoded:  "[]",
			expected: nil,
		},
		{
			name:    "single digest",
			encoded: `["sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1"]`,
			expected: []digest.Digest{
				digest.Digest("sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1"),
			},
		},
		{
			name:    "multiple digests",
			encoded: `["sha256:1111111111111111111111111111111111111111111111111111111111111111","sha256:2222222222222222222222222222222222222222222222222222222222222222"]`,
			expected: []digest.Digest{
				digest.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111"),
				digest.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
			},
		},
		{
			name:     "invalid json",
			encoded:  "not json",
			expected: nil,
		},
		{
			name:     "invalid digest in array",
			encoded:  `["not-a-digest"]`,
			expected: []digest.Digest{}, // Empty because invalid digest is skipped
		},
		{
			name:    "mixed valid and invalid",
			encoded: `["sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1", "invalid"]`,
			expected: []digest.Digest{
				digest.Digest("sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := DecodeLayerOrder(tc.encoded)

			if len(result) != len(tc.expected) {
				t.Fatalf("DecodeLayerOrder(%q) returned %d digests, want %d", tc.encoded, len(result), len(tc.expected))
			}

			for i, d := range result {
				if d != tc.expected[i] {
					t.Errorf("DecodeLayerOrder(%q)[%d] = %q, want %q", tc.encoded, i, d, tc.expected[i])
				}
			}
		})
	}
}

func TestLayerOrderRoundTrip(t *testing.T) {
	digests := []digest.Digest{
		digest.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111"),
		digest.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
		digest.Digest("sha256:3333333333333333333333333333333333333333333333333333333333333333"),
	}

	encoded := EncodeLayerOrder(digests)
	decoded := DecodeLayerOrder(encoded)

	if len(decoded) != len(digests) {
		t.Fatalf("round trip changed length: got %d, want %d", len(decoded), len(digests))
	}

	for i, d := range decoded {
		if d != digests[i] {
			t.Errorf("round trip[%d]: got %q, want %q", i, d, digests[i])
		}
	}
}

func TestMountTypeConstants(t *testing.T) {
	// Ensure mount type constants match expected values
	if MountTypeFormatErofs != "format/erofs" {
		t.Errorf("MountTypeFormatErofs = %q, want %q", MountTypeFormatErofs, "format/erofs")
	}
	if MountTypeErofs != "erofs" {
		t.Errorf("MountTypeErofs = %q, want %q", MountTypeErofs, "erofs")
	}
	if MountTypeExt4 != "ext4" {
		t.Errorf("MountTypeExt4 = %q, want %q", MountTypeExt4, "ext4")
	}
	if MountTypeBind != "bind" {
		t.Errorf("MountTypeBind = %q, want %q", MountTypeBind, "bind")
	}
}
