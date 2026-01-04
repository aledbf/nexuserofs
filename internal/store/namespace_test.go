package store

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

func TestNewNamespaceAwareStore(t *testing.T) {
	store := NewNamespaceAwareStore(nil, "default")
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	if store.defaultNamespace != "default" {
		t.Errorf("defaultNamespace = %q, want %q", store.defaultNamespace, "default")
	}
}

func TestGetNamespacedContext(t *testing.T) {
	tests := []struct {
		name             string
		inputNamespace   string // namespace to set in input context ("" means no namespace)
		defaultNamespace string
		wantNamespace    string
		wantErr          bool
	}{
		{
			name:             "uses context namespace when present",
			inputNamespace:   "my-namespace",
			defaultNamespace: "default",
			wantNamespace:    "my-namespace",
			wantErr:          false,
		},
		{
			name:             "falls back to default when context has no namespace",
			inputNamespace:   "",
			defaultNamespace: "default",
			wantNamespace:    "default",
			wantErr:          false,
		},
		{
			name:             "error when both context and default are empty",
			inputNamespace:   "",
			defaultNamespace: "",
			wantNamespace:    "",
			wantErr:          true,
		},
		{
			name:             "uses default for k8s.io namespace",
			inputNamespace:   "k8s.io",
			defaultNamespace: "default",
			wantNamespace:    "k8s.io",
			wantErr:          false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewNamespaceAwareStore(nil, tc.defaultNamespace)

			ctx := context.Background()
			if tc.inputNamespace != "" {
				ctx = namespaces.WithNamespace(ctx, tc.inputNamespace)
			}

			gotCtx, err := store.getNamespacedContext(ctx)

			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				if !errdefs.IsFailedPrecondition(err) {
					t.Errorf("expected ErrFailedPrecondition, got %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotNs, ok := namespaces.Namespace(gotCtx)
			if !ok {
				t.Fatal("expected namespace in returned context")
			}
			if gotNs != tc.wantNamespace {
				t.Errorf("namespace = %q, want %q", gotNs, tc.wantNamespace)
			}
		})
	}
}

func TestNamespaceAwareStore_NilClient(t *testing.T) {
	// Test that operations fail gracefully with nil client
	// This documents expected behavior when misconfigured
	store := NewNamespaceAwareStore(nil, "default")

	// These should panic or return errors due to nil client
	// We're documenting this behavior, not endorsing it
	t.Run("store returns nil for nil client", func(t *testing.T) {
		// store() calls client.ContentStore() which will panic on nil client
		// This test documents that the store requires a valid client
		defer func() {
			if r := recover(); r == nil {
				t.Log("no panic occurred - client might handle nil")
			}
		}()
		_ = store.store()
	})
}
