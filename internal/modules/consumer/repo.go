package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type repository struct {
	accounts   *mongo.Collection
	otp        *mongo.Collection
	refresh    *mongo.Collection
	addresses  *mongo.Collection
	wallets    *mongo.Collection
	walletTxns *mongo.Collection
	consents   *mongo.Collection
	payOrders  *mongo.Collection
	orders     *mongo.Collection
	deliveries *mongo.Collection
	// Operator collections — READ-mostly for the delivery flow (store/rider
	// resolution). Deliveries are consumer_*; these are shared Saathi identity.
	orgUnits        *mongo.Collection
	parties         *mongo.Collection
	roleAssignments *mongo.Collection
	notifications   *mongo.Collection
}

func newRepository(db *mongo.Database) *repository {
	return &repository{
		accounts:        db.Collection(collAccounts),
		otp:             db.Collection(collOTP),
		refresh:         db.Collection(collRefresh),
		addresses:       db.Collection(collAddresses),
		wallets:         db.Collection(collWallets),
		walletTxns:      db.Collection(collWalletTxns),
		consents:        db.Collection(collConsents),
		payOrders:       db.Collection(collPayOrders),
		orders:          db.Collection(collOrders),
		deliveries:      db.Collection(collDeliveries),
		orgUnits:        db.Collection("org_units"),
		parties:         db.Collection("parties"),
		roleAssignments: db.Collection("role_assignments"),
		notifications:   db.Collection("notifications"),
	}
}

// ensureIndexes creates the consumer collections' indexes at startup — the
// consumer module owns its own indexes so it never touches shared Saathi infra.
func (r *repository) ensureIndexes(ctx context.Context) error {
	specs := []struct {
		c    *mongo.Collection
		keys bson.D
		opts *options.IndexOptions
	}{
		{r.accounts, bson.D{{Key: "phone", Value: 1}}, options.Index().SetUnique(true)},
		{r.accounts, bson.D{{Key: "created_at", Value: -1}}, nil},
		{r.otp, bson.D{{Key: "phone", Value: 1}}, nil},
		{r.otp, bson.D{{Key: "expires_at", Value: 1}}, options.Index().SetExpireAfterSeconds(0)},
		{r.refresh, bson.D{{Key: "token_hash", Value: 1}}, options.Index().SetUnique(true)},
		{r.refresh, bson.D{{Key: "consumer_id", Value: 1}}, nil},
		{r.addresses, bson.D{{Key: "consumer_id", Value: 1}}, nil},
		{r.wallets, bson.D{{Key: "consumer_id", Value: 1}}, options.Index().SetUnique(true)},
		{r.walletTxns, bson.D{{Key: "consumer_id", Value: 1}, {Key: "seq", Value: -1}}, nil},
		// EXACTLY-ONCE money gate: a (consumer, ref, type) may exist at most once,
		// so a duplicate/concurrent same-ref movement can never double-credit.
		// Partial — only enforced for rows that actually carry a ref.
		{r.walletTxns, bson.D{{Key: "consumer_id", Value: 1}, {Key: "ref_id", Value: 1}, {Key: "type", Value: 1}},
			options.Index().SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "ref_id", Value: bson.D{{Key: "$gt", Value: ""}}}})},
		// A payment order id is globally unique — the top-up idempotency anchor.
		{r.payOrders, bson.D{{Key: "order_id", Value: 1}}, options.Index().SetUnique(true)},
		{r.payOrders, bson.D{{Key: "consumer_id", Value: 1}}, nil},
	}
	for _, s := range specs {
		if _, err := s.c.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: s.keys, Options: s.opts}); err != nil {
			return fmt.Errorf("consumer index (%v): %w", s.keys, err)
		}
	}
	if err := r.ensureOrderIndexes(ctx); err != nil {
		return fmt.Errorf("consumer order indexes: %w", err)
	}
	if err := r.ensureDeliveryIndexes(ctx); err != nil {
		return fmt.Errorf("consumer delivery indexes: %w", err)
	}
	return nil
}

// insertWalletTxnGate inserts a ledger row and reports whether it was a
// DUPLICATE (its unique ref+type already existed) — the atomic money gate:
// only the first insert of a (consumer, ref, type) proceeds to move money.
func (r *repository) insertWalletTxnGate(ctx context.Context, t walletTxn) (dup bool, err error) {
	if _, e := r.walletTxns.InsertOne(ctx, t); e != nil {
		if mongo.IsDuplicateKeyError(e) {
			return true, nil
		}
		return false, errInternal("wallet txn store failed")
	}
	return false, nil
}

// updateWalletTxnBalances backfills the wallet id, seq, and post-movement
// running balances onto the gate row after the atomic $inc returned them.
func (r *repository) updateWalletTxnBalances(ctx context.Context, id, walletID primitive.ObjectID, seq int64, cashAfter, rewardsAfter float64) {
	_, _ = r.walletTxns.UpdateByID(ctx, id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "wallet_id", Value: walletID}, {Key: "seq", Value: seq},
		{Key: "cash_after", Value: cashAfter}, {Key: "rewards_after", Value: rewardsAfter},
	}}})
}

func isNoDocs(err error) bool { return errors.Is(err, mongo.ErrNoDocuments) }

// ── Accounts ────────────────────────────────────────────────────────────────

func (r *repository) findAccountByPhone(ctx context.Context, phone string) (*account, error) {
	var a account
	err := r.accounts.FindOne(ctx, bson.D{{Key: "phone", Value: phone}}).Decode(&a)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("account lookup failed")
	}
	return &a, nil
}

func (r *repository) findAccountByID(ctx context.Context, id primitive.ObjectID) (*account, error) {
	var a account
	err := r.accounts.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&a)
	if isNoDocs(err) {
		return nil, errNotFound("account not found")
	}
	if err != nil {
		return nil, errInternal("account lookup failed")
	}
	return &a, nil
}

func (r *repository) insertAccount(ctx context.Context, a *account) error {
	if _, err := r.accounts.InsertOne(ctx, a); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("ALREADY_REGISTERED", "an account already exists for this phone")
		}
		return errInternal("account create failed")
	}
	return nil
}

func (r *repository) updateAccount(ctx context.Context, id primitive.ObjectID, set bson.D) (*account, error) {
	set = append(set, bson.E{Key: "updated_at", Value: time.Now().UTC()})
	after := options.After
	var a account
	err := r.accounts.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&a)
	if isNoDocs(err) {
		return nil, errNotFound("account not found")
	}
	if err != nil {
		return nil, errInternal("account update failed")
	}
	return &a, nil
}

func (r *repository) deleteAccountCascade(ctx context.Context, id primitive.ObjectID) error {
	// DPDP erasure: drop the consumer's PII rows. Financial ledgers would, in
	// production, be re-keyed to a pseudonymous id and retained (§21); for the
	// pilot we remove the consumer-scoped rows.
	filter := bson.D{{Key: "consumer_id", Value: id}}
	for _, c := range []*mongo.Collection{r.addresses, r.wallets, r.walletTxns, r.consents, r.refresh, r.payOrders} {
		if _, err := c.DeleteMany(ctx, filter); err != nil {
			return errInternal("erasure failed")
		}
	}
	// Orders carry raw PII (name, phone, precise geo, address) and are keyed by
	// user_id (the consumer hex), not consumer_id — erase them too.
	if _, err := r.orders.DeleteMany(ctx, bson.D{{Key: "user_id", Value: id.Hex()}}); err != nil {
		return errInternal("erasure failed")
	}
	// OTP challenges are keyed by phone, not consumer_id — clear by phone too so
	// no auth material or phone-number PII survives the erasure.
	if acct, err := r.findAccountByID(ctx, id); err == nil && acct != nil {
		phone := acct.Phone
		phone = phone[len(phone)-10:] // stored as +91XXXXXXXXXX → the OTP phone is the 10-digit
		_, _ = r.otp.DeleteMany(ctx, bson.D{{Key: "phone", Value: phone}})
	}
	if _, err := r.accounts.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
		return errInternal("erasure failed")
	}
	return nil
}

// ── OTP ─────────────────────────────────────────────────────────────────────

func (r *repository) insertOTP(ctx context.Context, c otpChallenge) error {
	if _, err := r.otp.InsertOne(ctx, c); err != nil {
		return errInternal("otp store failed")
	}
	return nil
}

func (r *repository) latestOTP(ctx context.Context, phone string) (*otpChallenge, error) {
	var c otpChallenge
	opts := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})
	err := r.otp.FindOne(ctx, bson.D{{Key: "phone", Value: phone}}, opts).Decode(&c)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("otp lookup failed")
	}
	return &c, nil
}

func (r *repository) deleteOTP(ctx context.Context, phone string) {
	_, _ = r.otp.DeleteMany(ctx, bson.D{{Key: "phone", Value: phone}})
}

// bumpOTPAttempt increments a challenge's attempt counter and returns the new
// count (best-effort; a read failure returns a high number to fail closed).
func (r *repository) bumpOTPAttempt(ctx context.Context, id string) int {
	after := options.After
	var ch otpChallenge
	err := r.otp.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&ch)
	if err != nil {
		return maxOTPAttempts
	}
	return ch.Attempts
}

// ── Refresh tokens ──────────────────────────────────────────────────────────

func (r *repository) insertRefresh(ctx context.Context, t refreshToken) error {
	if _, err := r.refresh.InsertOne(ctx, t); err != nil {
		return errInternal("refresh store failed")
	}
	return nil
}

func (r *repository) findRefresh(ctx context.Context, hash string) (*refreshToken, error) {
	var t refreshToken
	err := r.refresh.FindOne(ctx, bson.D{{Key: "token_hash", Value: hash}}).Decode(&t)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("refresh lookup failed")
	}
	return &t, nil
}

func (r *repository) deleteRefresh(ctx context.Context, hash string) (int64, error) {
	res, err := r.refresh.DeleteOne(ctx, bson.D{{Key: "token_hash", Value: hash}})
	if err != nil {
		return 0, errInternal("refresh delete failed")
	}
	return res.DeletedCount, nil
}

// ── Addresses ───────────────────────────────────────────────────────────────

func (r *repository) listAddresses(ctx context.Context, consumerID primitive.ObjectID) ([]address, error) {
	cur, err := r.addresses.Find(ctx, bson.D{{Key: "consumer_id", Value: consumerID}},
		options.Find().SetSort(bson.D{{Key: "is_default", Value: -1}, {Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, errInternal("addresses lookup failed")
	}
	out := []address{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("addresses decode failed")
	}
	return out, nil
}

func (r *repository) insertAddress(ctx context.Context, a *address) error {
	if _, err := r.addresses.InsertOne(ctx, a); err != nil {
		return errInternal("address create failed")
	}
	return nil
}

func (r *repository) updateAddress(ctx context.Context, id, consumerID primitive.ObjectID, set bson.D) (*address, error) {
	after := options.After
	var a address
	err := r.addresses.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "consumer_id", Value: consumerID}},
		bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&a)
	if isNoDocs(err) {
		return nil, errNotFound("address not found")
	}
	if err != nil {
		return nil, errInternal("address update failed")
	}
	return &a, nil
}

// clearDefaults unsets is_default on all of a consumer's addresses (single-default invariant).
func (r *repository) clearDefaults(ctx context.Context, consumerID primitive.ObjectID) error {
	_, err := r.addresses.UpdateMany(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "is_default", Value: false}}}})
	if err != nil {
		return errInternal("clear defaults failed")
	}
	return nil
}

func (r *repository) deleteAddress(ctx context.Context, id, consumerID primitive.ObjectID) (int64, error) {
	res, err := r.addresses.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "consumer_id", Value: consumerID}})
	if err != nil {
		return 0, errInternal("address delete failed")
	}
	return res.DeletedCount, nil
}

func (r *repository) findAddress(ctx context.Context, id, consumerID primitive.ObjectID) (*address, error) {
	var a address
	err := r.addresses.FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "consumer_id", Value: consumerID}}).Decode(&a)
	if isNoDocs(err) {
		return nil, errNotFound("address not found")
	}
	if err != nil {
		return nil, errInternal("address lookup failed")
	}
	return &a, nil
}

// ── Wallet ──────────────────────────────────────────────────────────────────

func (r *repository) findWallet(ctx context.Context, consumerID primitive.ObjectID) (*wallet, error) {
	var wl wallet
	err := r.wallets.FindOne(ctx, bson.D{{Key: "consumer_id", Value: consumerID}}).Decode(&wl)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("wallet lookup failed")
	}
	return &wl, nil
}

func (r *repository) insertWallet(ctx context.Context, wl *wallet) error {
	if _, err := r.wallets.InsertOne(ctx, wl); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil // created concurrently — fine
		}
		return errInternal("wallet create failed")
	}
	return nil
}

// incWallet ATOMICALLY increments the balances + seq via $inc and returns the
// fresh doc. Because $inc is atomic in Mongo, two concurrent money movements
// can never corrupt the balance (last-writer-wins is impossible) — the balance
// is the append-only ledger's materialised sum. Upserts so a missing wallet is
// created race-safe. seqDelta reserves the ledger seqs this movement will use.
func (r *repository) incWallet(ctx context.Context, consumerID primitive.ObjectID, cashDelta, rewardsDelta, heldDelta float64, seqDelta int64) (*wallet, error) {
	after := options.After
	var wl wallet
	err := r.wallets.FindOneAndUpdate(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}},
		bson.D{
			{Key: "$inc", Value: bson.D{
				{Key: "cash_balance", Value: cashDelta},
				{Key: "rewards_balance", Value: rewardsDelta},
				{Key: "held_amount", Value: heldDelta},
				{Key: "seq", Value: seqDelta},
			}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "_id", Value: primitive.NewObjectID()},
				{Key: "consumer_id", Value: consumerID},
				{Key: "currency", Value: "INR"},
			}},
		},
		options.FindOneAndUpdate().SetReturnDocument(after).SetUpsert(true),
	).Decode(&wl)
	if err != nil {
		return nil, errInternal("wallet update failed")
	}
	return &wl, nil
}

// debitWalletAtomic spends `amount` PROMO-FIRST (rewards preserved as spendable
// but real cash conserved last) in a SINGLE atomic guarded pipeline update. The
// $expr filter admits the update only when cash+rewards >= amount, so a
// concurrent debit can never overdraw and the insufficient-funds check is
// race-free. ok=false means insufficient funds (or no wallet) — nothing changed.
func (r *repository) debitWalletAtomic(ctx context.Context, consumerID primitive.ObjectID, amount float64, seqDelta int64) (*wallet, bool, error) {
	after := options.After
	var wl wallet
	err := r.wallets.FindOneAndUpdate(ctx,
		bson.D{
			{Key: "consumer_id", Value: consumerID},
			{Key: "$expr", Value: bson.D{{Key: "$gte", Value: bson.A{
				bson.D{{Key: "$add", Value: bson.A{"$cash_balance", "$rewards_balance"}}}, amount,
			}}}},
		},
		mongo.Pipeline{
			// __fp = amount taken from rewards (promo) first.
			bson.D{{Key: "$set", Value: bson.D{{Key: "__fp", Value: bson.D{{Key: "$min", Value: bson.A{amount, "$rewards_balance"}}}}}}},
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "rewards_balance", Value: bson.D{{Key: "$subtract", Value: bson.A{"$rewards_balance", "$__fp"}}}},
				{Key: "cash_balance", Value: bson.D{{Key: "$subtract", Value: bson.A{"$cash_balance", bson.D{{Key: "$subtract", Value: bson.A{amount, "$__fp"}}}}}}},
				{Key: "seq", Value: bson.D{{Key: "$add", Value: bson.A{"$seq", seqDelta}}}},
			}}},
			bson.D{{Key: "$unset", Value: "__fp"}},
		},
		options.FindOneAndUpdate().SetReturnDocument(after),
	).Decode(&wl)
	if isNoDocs(err) {
		return nil, false, nil // insufficient funds (or no wallet)
	}
	if err != nil {
		return nil, false, errInternal("wallet debit failed")
	}
	return &wl, true, nil
}

// deleteWalletTxn removes a ledger row by id — used to roll back a debit gate
// row when the guarded balance update finds insufficient funds.
func (r *repository) deleteWalletTxn(ctx context.Context, id primitive.ObjectID) {
	_, _ = r.walletTxns.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
}

func (r *repository) listWalletTxns(ctx context.Context, consumerID primitive.ObjectID, limit int64) ([]walletTxn, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cur, err := r.walletTxns.Find(ctx, bson.D{{Key: "consumer_id", Value: consumerID}},
		options.Find().SetSort(bson.D{{Key: "seq", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, errInternal("wallet txns lookup failed")
	}
	out := []walletTxn{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("wallet txns decode failed")
	}
	return out, nil
}

// ── Payment orders (Razorpay top-up) ────────────────────────────────────────

func (r *repository) insertPaymentOrder(ctx context.Context, o *paymentOrder) error {
	if _, err := r.payOrders.InsertOne(ctx, o); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return errConflict("ORDER_EXISTS", "payment order already exists")
		}
		return errInternal("payment order create failed")
	}
	return nil
}

// findPaymentOrder loads an order by its Razorpay id, scoped to the consumer so
// one shopper can never verify/settle another's order.
func (r *repository) findPaymentOrder(ctx context.Context, orderID string, consumerID primitive.ObjectID) (*paymentOrder, error) {
	var o paymentOrder
	err := r.payOrders.FindOne(ctx, bson.D{{Key: "order_id", Value: orderID}, {Key: "consumer_id", Value: consumerID}}).Decode(&o)
	if isNoDocs(err) {
		return nil, errNotFound("payment order not found")
	}
	if err != nil {
		return nil, errInternal("payment order lookup failed")
	}
	return &o, nil
}

// markPaymentOrderPaid flips CREATED→PAID exactly once and reports whether THIS
// call was the transition (guards a double credit even without the ledger gate).
func (r *repository) markPaymentOrderPaid(ctx context.Context, orderID, paymentID string, paidAt time.Time) (bool, error) {
	res, err := r.payOrders.UpdateOne(ctx,
		bson.D{{Key: "order_id", Value: orderID}, {Key: "status", Value: "CREATED"}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: "PAID"}, {Key: "payment_id", Value: paymentID}, {Key: "paid_at", Value: paidAt},
		}}})
	if err != nil {
		return false, errInternal("payment order update failed")
	}
	return res.ModifiedCount == 1, nil
}
