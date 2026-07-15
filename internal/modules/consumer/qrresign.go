package consumer

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// resignQRTokens repairs stored QR integrity tokens after a QR_SIGNING_SECRET
// change. Tokens are pure derivations of durable row fields + the secret —
// consignment batch QRs store HMAC(secret, batch_code)[:len], and product-lot
// QRs carry their signed payload INSIDE the token ("b64(payload).hexmac") — so
// re-signing is deterministic and idempotent: rows already signed with the
// current secret are untouched, rows signed under an older secret are updated
// in place. Without this, every QR minted before a secret rotation fails the
// integrity gate forever (409 QR_INTEGRITY_FAILED → the consumer app 404s).
//
// Runs once at boot, best-effort: a failure logs and never blocks startup
// (unlike the wallet index, nothing here guards money).
//
// Note: this blesses whatever rows exist under the current secret. That is the
// intent — integrity means "minted by this server", and anyone able to write
// these collections directly already owns the database.
func resignQRTokens(d *deps.Deps, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	secret := d.Cfg.QRSigningSecret

	fixedBatch := resignConsignmentBatchQRs(ctx, d.DB.Collection(mongodb.CollConsignmentBatchQRs), secret, log)
	fixedProduct := resignProductQRs(ctx, d.DB.Collection(mongodb.CollBatchQRs), secret, log)
	if fixedBatch+fixedProduct > 0 {
		log.Info("qr tokens re-signed under the current QR secret",
			slog.Int("consignment_batch_qrs", fixedBatch),
			slog.Int("batch_qrs", fixedProduct))
	}
}

// resignConsignmentBatchQRs re-derives token = HMAC(secret, batch_code)[:len]
// for every per-samiti batch QR, preserving each row's stored token length.
func resignConsignmentBatchQRs(ctx context.Context, coll *mongo.Collection, secret string, log *slog.Logger) int {
	cur, err := coll.Find(ctx, bson.M{})
	if err != nil {
		log.Warn("qr resign: list consignment batch qrs", slog.Any("err", err))
		return 0
	}
	defer cur.Close(ctx)

	fixed := 0
	for cur.Next(ctx) {
		var row struct {
			ID        primitive.ObjectID `bson:"_id"`
			BatchCode string             `bson:"batch_code"`
			Token     string             `bson:"token"`
		}
		if err := cur.Decode(&row); err != nil || row.BatchCode == "" {
			continue
		}
		tokenLen := len(row.Token)
		if tokenLen == 0 {
			tokenLen = 8 // the mint length (quality module batchQRTokenLen)
		}
		full := auth.HMACHash(secret, row.BatchCode)
		if tokenLen > len(full) {
			tokenLen = len(full)
		}
		expected := full[:tokenLen]
		if auth.ConstantTimeEqual(expected, row.Token) {
			continue // already signed with the current secret
		}
		if _, err := coll.UpdateByID(ctx, row.ID, bson.M{"$set": bson.M{"token": expected}}); err != nil {
			log.Warn("qr resign: update consignment batch qr",
				slog.String("batch_code", row.BatchCode), slog.Any("err", err))
			continue
		}
		fixed++
	}
	return fixed
}

// resignProductQRs recomputes the HMAC half of a product-lot signed token.
// The signed payload (qr_code|lot_hex|issued_unix) travels base64url-encoded
// inside the token itself, so the signature is recomputable without touching
// any other field: keep the payload, replace the mac.
func resignProductQRs(ctx context.Context, coll *mongo.Collection, secret string, log *slog.Logger) int {
	cur, err := coll.Find(ctx, bson.M{})
	if err != nil {
		log.Warn("qr resign: list product qrs", slog.Any("err", err))
		return 0
	}
	defer cur.Close(ctx)

	fixed := 0
	for cur.Next(ctx) {
		var row struct {
			ID          primitive.ObjectID `bson:"_id"`
			SignedToken string             `bson:"signed_token"`
		}
		if err := cur.Decode(&row); err != nil {
			continue
		}
		payloadB64, sig, found := strings.Cut(row.SignedToken, ".")
		if !found || payloadB64 == "" {
			continue // malformed/legacy row — leave for manual review
		}
		payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
		if err != nil || len(payload) == 0 {
			continue
		}
		expected := auth.HMACHash(secret, string(payload))
		if auth.ConstantTimeEqual(expected, sig) {
			continue // already signed with the current secret
		}
		token := payloadB64 + "." + expected
		if _, err := coll.UpdateByID(ctx, row.ID, bson.M{"$set": bson.M{"signed_token": token}}); err != nil {
			log.Warn("qr resign: update product qr", slog.Any("err", err))
			continue
		}
		fixed++
	}
	return fixed
}
