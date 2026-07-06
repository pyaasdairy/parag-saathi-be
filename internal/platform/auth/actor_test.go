package auth

import (
	"context"
	"testing"
)

// TestActorHolderPropagatesOutward pins the mechanism the audit middleware
// relies on: identity installed by INNER middleware (WithActor) must be
// observable from the OUTER frame that installed the holder — otherwise
// every HTTP mutation audit entry records an empty actor.
func TestActorHolderPropagatesOutward(t *testing.T) {
	outer := WithActorHolder(context.Background())

	// Before authentication runs, the holder is empty.
	if _, ok := HeldActor(outer); ok {
		t.Fatal("HeldActor must be empty before WithActor runs")
	}

	// Inner middleware derives a child context — the holder pointer is shared.
	actor := Actor{PartyID: "party-1", RoleCode: "SUPER_ADMIN"}
	inner := WithActor(outer, actor)

	got, ok := HeldActor(outer)
	if !ok {
		t.Fatal("HeldActor(outer) must observe the actor set on the inner context")
	}
	if got.PartyID != "party-1" || got.RoleCode != "SUPER_ADMIN" {
		t.Fatalf("held actor = %+v, want party-1/SUPER_ADMIN", got)
	}

	// The regular inward lookup still works on the inner context.
	if a, ok := ActorFrom(inner); !ok || a.PartyID != "party-1" {
		t.Fatalf("ActorFrom(inner) = %+v, %v", a, ok)
	}

	// Without a holder installed, WithActor is a no-op for HeldActor.
	plain := WithActor(context.Background(), actor)
	if _, ok := HeldActor(plain); ok {
		t.Fatal("HeldActor must report false when no holder was installed")
	}
}
