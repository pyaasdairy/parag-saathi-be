package cattle

import (
	"net/http"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

func TestValidatePashuAadhaar(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid 12 digits", input: "123456789012", wantErr: false},
		{name: "valid all zeros", input: "000000000000", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "too short", input: "12345678901", wantErr: true},
		{name: "too long", input: "1234567890123", wantErr: true},
		{name: "contains letter", input: "12345678901A", wantErr: true},
		{name: "contains space", input: "123456 89012", wantErr: true},
		{name: "contains hyphen", input: "1234-5678901", wantErr: true},
		{name: "unicode digits rejected", input: "١٢٣٤٥٦٧٨٩٠١٢", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePashuAadhaar(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePashuAadhaar(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err != nil {
				if err.Status != http.StatusBadRequest {
					t.Fatalf("validatePashuAadhaar(%q) status = %d, want %d", tt.input, err.Status, http.StatusBadRequest)
				}
				if err.Code != "INVALID_PASHU_AADHAAR" {
					t.Fatalf("validatePashuAadhaar(%q) code = %q, want INVALID_PASHU_AADHAAR", tt.input, err.Code)
				}
			}
		})
	}
}

func TestResolveAnimalOwner(t *testing.T) {
	farmerID := primitive.NewObjectID()
	sacheevID := primitive.NewObjectID()
	vetID := primitive.NewObjectID()
	otherFarmerID := primitive.NewObjectID()
	behalfFarmerID := primitive.NewObjectID()

	farmer := auth.Actor{PartyID: farmerID.Hex(), RoleCode: domain.RoleFarmer}
	sacheev := auth.Actor{PartyID: sacheevID.Hex(), RoleCode: domain.RoleSamitiSacheev}
	vet := auth.Actor{PartyID: vetID.Hex(), RoleCode: domain.RoleVeterinarian}

	tests := []struct {
		name           string
		actor          auth.Actor
		actorID        primitive.ObjectID
		requestedOwner *primitive.ObjectID
		wantOwner      primitive.ObjectID
		wantStatus     int    // 0 = no error expected
		wantCode       string // checked only when wantStatus != 0
	}{
		{
			name:           "farmer omitting owner registers self",
			actor:          farmer,
			actorID:        farmerID,
			requestedOwner: nil,
			wantOwner:      farmerID,
		},
		{
			name:           "farmer naming own party id registers self",
			actor:          farmer,
			actorID:        farmerID,
			requestedOwner: &farmerID,
			wantOwner:      farmerID,
		},
		{
			name:           "farmer naming another party is forbidden",
			actor:          farmer,
			actorID:        farmerID,
			requestedOwner: &otherFarmerID,
			wantStatus:     http.StatusForbidden,
			wantCode:       "FORBIDDEN",
		},
		{
			name:           "sacheev must name the owner",
			actor:          sacheev,
			actorID:        sacheevID,
			requestedOwner: nil,
			wantStatus:     http.StatusBadRequest,
			wantCode:       "OWNER_REQUIRED",
		},
		{
			name:           "sacheev naming an owner registers on their behalf",
			actor:          sacheev,
			actorID:        sacheevID,
			requestedOwner: &behalfFarmerID,
			wantOwner:      behalfFarmerID,
		},
		{
			name:           "veterinarian naming an owner registers on their behalf",
			actor:          vet,
			actorID:        vetID,
			requestedOwner: &behalfFarmerID,
			wantOwner:      behalfFarmerID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, err := resolveAnimalOwner(tt.actor, tt.actorID, tt.requestedOwner)
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("resolveAnimalOwner() unexpected error: %v", err)
				}
				if owner != tt.wantOwner {
					t.Fatalf("resolveAnimalOwner() owner = %q, want %q", owner.Hex(), tt.wantOwner.Hex())
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveAnimalOwner() expected error, got owner %q", owner.Hex())
			}
			if err.Status != tt.wantStatus {
				t.Fatalf("resolveAnimalOwner() status = %d, want %d", err.Status, tt.wantStatus)
			}
			if err.Code != tt.wantCode {
				t.Fatalf("resolveAnimalOwner() code = %q, want %q", err.Code, tt.wantCode)
			}
		})
	}
}

func TestCanViewAnimal(t *testing.T) {
	ownerID := primitive.NewObjectID()
	otherFarmerID := primitive.NewObjectID()
	vetID := primitive.NewObjectID()
	moID := primitive.NewObjectID()
	animal := &domain.Animal{ID: primitive.NewObjectID(), OwnerPartyID: ownerID}

	tests := []struct {
		name      string
		actor     auth.Actor
		actorID   primitive.ObjectID
		wantAllow bool
	}{
		{
			name:      "owner farmer may view",
			actor:     auth.Actor{PartyID: ownerID.Hex(), RoleCode: domain.RoleFarmer},
			actorID:   ownerID,
			wantAllow: true,
		},
		{
			name:      "non-owner farmer is forbidden",
			actor:     auth.Actor{PartyID: otherFarmerID.Hex(), RoleCode: domain.RoleFarmer},
			actorID:   otherFarmerID,
			wantAllow: false,
		},
		{
			name:      "veterinarian may view (v1 role check; consent TODO)",
			actor:     auth.Actor{PartyID: vetID.Hex(), RoleCode: domain.RoleVeterinarian},
			actorID:   vetID,
			wantAllow: true,
		},
		{
			name:      "mission official may view (v1 role check; consent TODO)",
			actor:     auth.Actor{PartyID: moID.Hex(), RoleCode: domain.RoleMissionOfficial},
			actorID:   moID,
			wantAllow: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := canViewAnimal(tt.actor, tt.actorID, animal)
			if tt.wantAllow && err != nil {
				t.Fatalf("canViewAnimal() unexpected error: %v", err)
			}
			if !tt.wantAllow {
				if err == nil {
					t.Fatal("canViewAnimal() expected forbidden, got nil")
				}
				if err.Status != http.StatusForbidden {
					t.Fatalf("canViewAnimal() status = %d, want %d", err.Status, http.StatusForbidden)
				}
			}
		})
	}
}
