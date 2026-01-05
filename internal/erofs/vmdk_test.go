package erofsutils

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteVMDKDescriptor(t *testing.T) {
	// Create temporary test files
	tmpDir := t.TempDir()

	// Create test device files with known sizes
	device1 := filepath.Join(tmpDir, "layer1.erofs")
	device2 := filepath.Join(tmpDir, "layer2.erofs")

	// Create 1MB and 2MB files (sizes must be multiples of 512 bytes)
	if err := os.WriteFile(device1, make([]byte, 1024*1024), 0644); err != nil {
		t.Fatalf("failed to create device1: %v", err)
	}
	if err := os.WriteFile(device2, make([]byte, 2*1024*1024), 0644); err != nil {
		t.Fatalf("failed to create device2: %v", err)
	}

	var buf bytes.Buffer
	err := WriteVMDKDescriptor(&buf, []string{device1, device2})
	if err != nil {
		t.Fatalf("WriteVMDKDescriptor failed: %v", err)
	}

	result := buf.String()

	// Verify header
	if !strings.Contains(result, "# Disk DescriptorFile") {
		t.Error("missing VMDK header")
	}
	if !strings.Contains(result, "version=1") {
		t.Error("missing version")
	}
	if !strings.Contains(result, `createType="twoGbMaxExtentFlat"`) {
		t.Error("missing createType")
	}

	// Verify extents - 1MB = 2048 sectors, 2MB = 4096 sectors
	if !strings.Contains(result, "RW 2048 FLAT") {
		t.Error("missing extent for device1 (1MB = 2048 sectors)")
	}
	if !strings.Contains(result, "RW 4096 FLAT") {
		t.Error("missing extent for device2 (2MB = 4096 sectors)")
	}

	// Verify device paths are included
	if !strings.Contains(result, device1) {
		t.Errorf("missing device1 path in descriptor: %s", device1)
	}
	if !strings.Contains(result, device2) {
		t.Errorf("missing device2 path in descriptor: %s", device2)
	}

	// Verify DDB section
	if !strings.Contains(result, "ddb.virtualHWVersion") {
		t.Error("missing ddb.virtualHWVersion")
	}
	if !strings.Contains(result, "ddb.geometry.cylinders") {
		t.Error("missing ddb.geometry.cylinders")
	}
}

func TestWriteVMDKDescriptorToFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test device file
	device := filepath.Join(tmpDir, "layer.erofs")
	if err := os.WriteFile(device, make([]byte, 1024*1024), 0644); err != nil {
		t.Fatalf("failed to create device: %v", err)
	}

	vmdkPath := filepath.Join(tmpDir, "merged.vmdk")
	err := WriteVMDKDescriptorToFile(vmdkPath, []string{device})
	if err != nil {
		t.Fatalf("WriteVMDKDescriptorToFile failed: %v", err)
	}

	// Verify file exists and has content
	content, err := os.ReadFile(vmdkPath)
	if err != nil {
		t.Fatalf("failed to read VMDK file: %v", err)
	}

	if len(content) == 0 {
		t.Error("VMDK file is empty")
	}

	if !strings.Contains(string(content), "# Disk DescriptorFile") {
		t.Error("VMDK file missing header")
	}
}

func TestWriteVMDKDescriptor_NonexistentDevice(t *testing.T) {
	var buf bytes.Buffer
	err := WriteVMDKDescriptor(&buf, []string{"/nonexistent/device.erofs"})
	if err == nil {
		t.Error("expected error for nonexistent device")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestWriteVMDKDescriptor_EmptyDevice(t *testing.T) {
	tmpDir := t.TempDir()

	// Create empty file
	device := filepath.Join(tmpDir, "empty.erofs")
	if err := os.WriteFile(device, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create empty device: %v", err)
	}

	var buf bytes.Buffer
	err := WriteVMDKDescriptor(&buf, []string{device})
	if err == nil {
		t.Error("expected error for empty device")
	}
	if !strings.Contains(err.Error(), "zero size") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestWriteVMDKDescriptor_TinyDevice(t *testing.T) {
	tmpDir := t.TempDir()

	// Create file smaller than 512 bytes
	device := filepath.Join(tmpDir, "tiny.erofs")
	if err := os.WriteFile(device, make([]byte, 100), 0644); err != nil {
		t.Fatalf("failed to create tiny device: %v", err)
	}

	var buf bytes.Buffer
	err := WriteVMDKDescriptor(&buf, []string{device})
	if err == nil {
		t.Error("expected error for device smaller than 512 bytes")
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestVmdkDescAddExtent_LargeDevice(t *testing.T) {
	// Test extent splitting for devices larger than 2GB
	var buf bytes.Buffer

	// Simulate a 3GB device (6291456 sectors)
	// This should be split into two extents
	sectors := uint64(6291456) // 3GB in 512-byte sectors
	err := vmdkDescAddExtent(&buf, sectors, "/path/to/device", 0)
	if err != nil {
		t.Fatalf("vmdkDescAddExtent failed: %v", err)
	}

	result := buf.String()
	lines := strings.Split(strings.TrimSpace(result), "\n")

	// Should have 2 extent lines (2GB + 1GB)
	if len(lines) != 2 {
		t.Errorf("expected 2 extent lines for 3GB device, got %d", len(lines))
	}

	// First extent should be max2GbExtentSectors (4194304)
	if !strings.Contains(lines[0], "RW 4194304 FLAT") {
		t.Errorf("first extent should be 4194304 sectors, got: %s", lines[0])
	}

	// Second extent should be remaining sectors
	if !strings.Contains(lines[1], "RW 2097152 FLAT") {
		t.Errorf("second extent should be 2097152 sectors, got: %s", lines[1])
	}
}
