package cattle

import (
	"net/http"
	"testing"

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
	farmer := auth.Actor{PartyID: "party-farmer-1", RoleCode: domain.RoleFarmer}
	sacheev := auth.Actor{PartyID: "party-sacheev-1", RoleCode: domain.RoleSamitiSacheev}
	vet := auth.Actor{PartyID: "party-vet-1", RoleCode: domain.RoleVeterinarian}

	tests := []struct {
		name           string
		actor          auth.Actor
		requestedOwner string
		wantOwner      string
		wantStatus     int    // 0 = no error expected
		wantCode       string // checked only when wantStatus != 0
	}{
		{
			name:           "farmer omitting owner registers self",
			actor:          farmer,
			requestedOwner: "",
			wantOwner:      "party-farmer-1",
		},
		{
			name:           "farmer naming own party id registers self",
			actor:          farmer,
			requestedOwner: "party-farmer-1",
			wantOwner:      "party-farmer-1",
		},
		{
			name:           "farmer naming another party is forbidden",
			actor:          farmer,
			requestedOwner: "party-other-9",
			wantStatus:     http.StatusForbidden,
			wantCode:       "FORBIDDEN",
		},
		{
			name:           "sacheev must name the owner",
			actor:          sacheev,
			requestedOwner: "",
			wantStatus:     http.StatusBadRequest,
			wantCode:       "OWNER_REQUIRED",
		},
		{
			name:           "sacheev naming an owner registers on their behalf",
			actor:          sacheev,
			requestedOwner: "party-farmer-2",
			wantOwner:      "party-farmer-2",
		},
		{
			name:           "veterinarian naming an owner registers on their behalf",
			actor:          vet,
			requestedOwner: "party-farmer-3",
			wantOwner:      "party-farmer-3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, err := resolveAnimalOwner(tt.actor, tt.requestedOwner)
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("resolveAnimalOwner() unexpected error: %v", err)
				}
				if owner != tt.wantOwner {
					t.Fatalf("resolveAnimalOwner() owner = %q, want %q", owner, tt.wantOwner)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveAnimalOwner() expected error, got owner %q", owner)
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
	animal := &domain.Animal{ID: "animal-1", OwnerPartyID: "party-farmer-1"}

	tests := []struct {
		name      string
		actor     auth.Actor
		wantAllow bool
	}{
		{
			name:      "owner farmer may view",
			actor:     auth.Actor{PartyID: "party-farmer-1", RoleCode: domain.RoleFarmer},
			wantAllow: true,
		},
		{
			name:      "non-owner farmer is forbidden",
			actor:     auth.Actor{PartyID: "party-farmer-2", RoleCode: domain.RoleFarmer},
			wantAllow: false,
		},
		{
			name:      "veterinarian may view (v1 role check; consent TODO)",
			actor:     auth.Actor{PartyID: "party-vet-1", RoleCode: domain.RoleVeterinarian},
			wantAllow: true,
		},
		{
			name:      "mission official may view (v1 role check; consent TODO)",
			actor:     auth.Actor{PartyID: "party-mo-1", RoleCode: domain.RoleMissionOfficial},
			wantAllow: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := canViewAnimal(tt.actor, animal)
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
