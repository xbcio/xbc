package tasks

import (
	"errors"
	"fmt"
	"testing"
)

// TestPermanentWrapsAndIsIdempotent pins the three promises Permanent makes:
// nil stays nil, the marked error reaches both the cause and ErrPermanent, and
// marking twice returns the already marked value unchanged.
func TestPermanentWrapsAndIsIdempotent(t *testing.T) {
	if got := Permanent(nil); got != nil {
		t.Fatalf("Permanent(nil) = %v, want nil", got)
	}

	cause := errors.New("missing recipient")
	marked := Permanent(cause)
	if !errors.Is(marked, ErrPermanent) {
		t.Fatalf("errors.Is(Permanent(cause), ErrPermanent) = false")
	}
	if !errors.Is(marked, cause) {
		t.Fatalf("errors.Is(Permanent(cause), cause) = false")
	}
	if marked.Error() != cause.Error() {
		t.Fatalf("Permanent(cause).Error() = %q, want the cause's text %q", marked.Error(), cause.Error())
	}
	if errors.Is(cause, ErrPermanent) {
		t.Fatal("marking must not change the cause itself")
	}

	if again := Permanent(marked); again != marked {
		t.Fatalf("Permanent(marked) = %v, want the same value back", again)
	}
	wrapped := fmt.Errorf("send: %w", marked)
	if again := Permanent(wrapped); again != wrapped {
		t.Fatalf("Permanent(wrapped marked) = %v, want the same value back", again)
	}
}
