package permissions

import (
	"errors"
	"testing"
)

func TestSetHas(t *testing.T) {
	s := NewSet(CapRead, CapSend)
	if !s.Has(CapRead) {
		t.Error("expected CapRead granted")
	}
	if !s.Has(CapSend) {
		t.Error("expected CapSend granted")
	}
	if s.Has(CapDelete) {
		t.Error("did not expect CapDelete granted")
	}
}

func TestNewSetFromStrings(t *testing.T) {
	s, unknown := NewSetFromStrings([]string{"read", "send", "READ", "  send  "})
	if len(unknown) != 0 {
		t.Errorf("unexpected unknown entries: %v", unknown)
	}
	if !s.Has(CapRead) || !s.Has(CapSend) {
		t.Errorf("expected read+send granted, got %s", s)
	}
	if s.Has(CapModify) || s.Has(CapDelete) {
		t.Errorf("unexpected capabilities granted: %s", s)
	}
}

func TestNewSetFromStringsUnknown(t *testing.T) {
	s, unknown := NewSetFromStrings([]string{"read", "delete-everything", "  drop  "})
	if len(unknown) != 2 {
		t.Errorf("expected 2 unknown entries, got %d: %v", len(unknown), unknown)
	}
	if !s.Has(CapRead) {
		t.Error("expected CapRead granted from known entry")
	}
	if s.Has(CapModify) {
		t.Error("did not expect CapModify granted")
	}
}

func TestEmptySet(t *testing.T) {
	var s Set
	if s.Has(CapRead) {
		t.Error("nil set should not grant anything")
	}
	if !s.IsEmpty() {
		t.Error("nil set should be empty")
	}
}

func TestListSorted(t *testing.T) {
	s := NewSet(CapDelete, CapRead, CapSend)
	got := s.List()
	want := []Capability{CapDelete, CapRead, CapSend}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestString(t *testing.T) {
	s := NewSet(CapDelete, CapRead, CapSend)
	got := s.String()
	want := "delete,read,send"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestCheckPasses(t *testing.T) {
	s := NewSet(CapRead)
	if err := Check("personal", s, CapRead); err != nil {
		t.Errorf("Check(read) = %v, want nil", err)
	}
}

func TestCheckFails(t *testing.T) {
	s := NewSet(CapRead)
	err := Check("personal", s, CapSend)
	if err == nil {
		t.Fatal("Check(send) = nil, want error")
	}
	var pde *ErrPermissionDenied
	if !errors.As(err, &pde) {
		t.Fatalf("err is not *ErrPermissionDenied: %T", err)
	}
	if pde.Account != "personal" || pde.Capability != CapSend {
		t.Errorf("got %+v, want account=personal capability=send", pde)
	}
	if msg := err.Error(); msg == "" {
		t.Error("Error() returned empty string")
	}
}
