package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// QCCertificate is a first-class, issued safety-gate certificate for a
// processing batch whose PLANT_LAB QC has PASSED (blueprint §8.3). Unlike the
// certificate number stamped inline on a passing QCResult, this is a durable,
// independently addressable document: it rosters the underlying test-result
// IDs, records the issuing analyst and the plant's FSSAI licence, and can be
// presented as the compliance artefact for the batch's finished product.
type QCCertificate struct {
	ID                primitive.ObjectID `bson:"_id,omitempty"      json:"id"`
	BatchID           primitive.ObjectID `bson:"batch_id"           json:"batch_id"`
	CertificateNumber string             `bson:"certificate_number" json:"certificate_number"` // unique business key
	// TestResultIDs is the roster of QCResult documents this certificate
	// attests to (the batch's recorded results at issuance time).
	TestResultIDs   []primitive.ObjectID `bson:"test_result_ids"       json:"test_result_ids"`
	FSSAILicNo      string               `bson:"fssai_lic_no,omitempty" json:"fssai_lic_no,omitempty"`
	IssuedByPartyID primitive.ObjectID   `bson:"issued_by_party_id"    json:"issued_by_party_id"`
	IssuedAt        time.Time            `bson:"issued_at"             json:"issued_at"`
}
