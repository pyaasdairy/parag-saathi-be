package settlement

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

func TestValidateApproval(t *testing.T) {
	sacheev := primitive.NewObjectID()
	adhyaksh := primitive.NewObjectID()
	superAdmin := primitive.NewObjectID()

	tests := []struct {
		name       string
		batch      domain.SettlementBatch
		approver   primitive.ObjectID
		wantStatus int    // 0 = no error expected
		wantReason string // expected details.reason, "" = don't check
	}{
		{
			name: "different approver on pending batch passes",
			batch: domain.SettlementBatch{
				InitiatedBy: sacheev,
				Status:      domain.SettlementStatusPendingApproval,
			},
			approver: adhyaksh,
		},
		{
			name: "initiator approving own batch is a dual-control violation",
			batch: domain.SettlementBatch{
				InitiatedBy: sacheev,
				Status:      domain.SettlementStatusPendingApproval,
			},
			approver:   sacheev,
			wantStatus: http.StatusForbidden,
			wantReason: "DUAL_CONTROL_VIOLATION",
		},
		{
			name: "dual control is checked before status — even a super-admin initiator on an approved batch is refused as a violation",
			batch: domain.SettlementBatch{
				InitiatedBy: superAdmin,
				Status:      domain.SettlementStatusApproved,
			},
			approver:   superAdmin,
			wantStatus: http.StatusForbidden,
			wantReason: "DUAL_CONTROL_VIOLATION",
		},
		{
			name: "already approved batch conflicts",
			batch: domain.SettlementBatch{
				InitiatedBy: sacheev,
				Status:      domain.SettlementStatusApproved,
			},
			approver:   adhyaksh,
			wantStatus: http.StatusConflict,
		},
		{
			name: "rejected batch conflicts",
			batch: domain.SettlementBatch{
				InitiatedBy: sacheev,
				Status:      domain.SettlementStatusRejected,
			},
			approver:   adhyaksh,
			wantStatus: http.StatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateApproval(tt.batch, tt.approver)
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			var ae *httpx.AppError
			if !errors.As(err, &ae) {
				t.Fatalf("expected *httpx.AppError, got %T (%v)", err, err)
			}
			if ae.Status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", ae.Status, tt.wantStatus)
			}
			if tt.wantReason != "" {
				details, ok := ae.Details.(map[string]string)
				if !ok || details["reason"] != tt.wantReason {
					t.Fatalf("details = %#v, want reason %q", ae.Details, tt.wantReason)
				}
			}
		})
	}
}

func TestValidateExecution(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantStatus int // 0 = no error expected
	}{
		{name: "approved batch may execute", status: domain.SettlementStatusApproved},
		{name: "pending batch may not execute before approval", status: domain.SettlementStatusPendingApproval, wantStatus: http.StatusConflict},
		{name: "initiated batch may not execute", status: domain.SettlementStatusInitiated, wantStatus: http.StatusConflict},
		{name: "rejected batch may not execute", status: domain.SettlementStatusRejected, wantStatus: http.StatusConflict},
		{name: "executed batch may not execute again", status: domain.SettlementStatusExecuted, wantStatus: http.StatusConflict},
		// EXECUTING and FAILED may re-enter execution: the interrupted-run
		// resume path (claimExecution's lease guards against double-execute).
		{name: "executing batch may be resumed after an interrupted run", status: domain.SettlementStatusExecuting},
		{name: "failed batch may be re-executed", status: domain.SettlementStatusFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExecution(domain.SettlementBatch{Status: tt.status})
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			var ae *httpx.AppError
			if !errors.As(err, &ae) {
				t.Fatalf("expected *httpx.AppError, got %T (%v)", err, err)
			}
			if ae.Status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", ae.Status, tt.wantStatus)
			}
		})
	}
}

func TestMockReferences(t *testing.T) {
	if utr := mockUTR(); !regexp.MustCompile(`^UTRMOCK[0-9A-F]{12}$`).MatchString(utr) {
		t.Fatalf("mockUTR() = %q, want UTRMOCK followed by 12 uppercase hex chars", utr)
	}
	if ref := mockPARef(); !strings.HasPrefix(ref, "PA-MOCK-") || len(ref) != len("PA-MOCK-")+8 {
		t.Fatalf("mockPARef() = %q, want PA-MOCK- plus 8 chars", ref)
	}
	if ref := mockPFMSRef(); !strings.HasPrefix(ref, "PFMS-MOCK-") || len(ref) != len("PFMS-MOCK-")+8 {
		t.Fatalf("mockPFMSRef() = %q, want PFMS-MOCK- plus 8 chars", ref)
	}
}
