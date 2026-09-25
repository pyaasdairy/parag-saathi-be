package consumer

// REFER A FRIEND, MOVE UP THE LINE (spec 5.8 and section 6; the app's
// Founding Family screen: "Each friend who joins moves you up"; founder,
// 25 Sep: keep the Rs 100 referral credit AND add this line movement).
//
// THE RULE
//   - It counts when the referred friend pays the Rs 99: a Founding Family
//     join that moved money (from cash, or from the friend's own referral
//     credit), once per referral, ever. A friend who stops and joins again
//     moves nobody a second time.
//   - The referrer moves up ONE place on their own farm's line: they swap
//     line numbers with the waiting member directly ahead of them (the
//     waiting member of the same farm holding the largest number below
//     theirs). The friend may have claimed any farm.
//   - Only a WAITING referrer on a farm still filling moves. An active member
//     (their farm has unlocked: the line is over), a stopped member, a family
//     that is not a member and a referrer already first in line are
//     unaffected. The Rs 100 credit (referrals.go) is untouched either way.
//
// UNIQUE NUMBERS. A swap keeps every number held by exactly one member and
// never touches next_line (takeFarmLine), so line numbers stay unique and
// next_line stays monotonic. Swaps on one farm run one at a time under a
// short lease on the farm row (line_lock_until / line_lock_by). The planned
// swap is written on the referral before either member moves, so a process
// that dies between the two single-row updates is finished, idempotently,
// by the next holder of that farm's lease (finishPlannedLineMoves) or by
// the referral worker's sweep (applyDueLineMoves).
//
// The referrer is told by FF-04 (founding.line_moved), the spec section 6
// copy: "Your friend Neha joined. You moved up to #271. She claimed Gonard
// Dairy: 2 more to unlock."

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	lineMoveMoved      = "moved"       // the referrer moved up one place
	lineMoveNotWaiting = "not_waiting" // the referrer was not waiting on a filling farm
	lineMoveFront      = "front"       // the referrer was already first in line

	// farmLineLease bounds how long one swap may hold a farm's line.
	farmLineLease = 30 * time.Second
)

// referralLineMove is the line movement a referral earned, on the referral
// row. Result is "" while the swap is planned and not yet finished.
type referralLineMove struct {
	Result   string             `bson:"result,omitempty"`
	FarmID   string             `bson:"farm_id,omitempty"`
	MemberID primitive.ObjectID `bson:"member_id,omitempty"`
	From     int                `bson:"from,omitempty"`
	AheadID  primitive.ObjectID `bson:"ahead_id,omitempty"`
	To       int                `bson:"to,omitempty"`
	At       time.Time          `bson:"at"`
}

// ── Repo ────────────────────────────────────────────────────────────────────

// markReferralLineMoveDue records, once per referral, that the referee paid
// the Rs 99 (the friend's farm is kept for the message). nil when the
// referee has no referral or its line movement was already earned.
func (r *repository) markReferralLineMoveDue(ctx context.Context, refereeID primitive.ObjectID, friendFarm string, at time.Time) (*referral, error) {
	after := options.After
	var ref referral
	err := r.referrals().FindOneAndUpdate(ctx,
		bson.D{
			{Key: "referee_id", Value: refereeID},
			{Key: "line_move_due_at", Value: bson.D{{Key: "$exists", Value: false}}},
			{Key: "line_move", Value: bson.D{{Key: "$exists", Value: false}}},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "line_move_due_at", Value: at}, {Key: "line_move_friend_farm", Value: friendFarm}}}},
		options.FindOneAndUpdate().SetReturnDocument(after)).Decode(&ref)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("referral update failed")
	}
	return &ref, nil
}

func (r *repository) findReferralByID(ctx context.Context, id primitive.ObjectID) (*referral, error) {
	var ref referral
	err := r.referrals().FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&ref)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("referral lookup failed")
	}
	return &ref, nil
}

// listDueLineMoves: referrals whose line movement is due and not finished.
func (r *repository) listDueLineMoves(ctx context.Context, now time.Time) ([]referral, error) {
	cur, err := r.referrals().Find(ctx, bson.D{
		{Key: "line_move_due_at", Value: bson.D{{Key: "$lte", Value: now}}},
		{Key: "line_move.result", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}, options.Find().SetSort(bson.D{{Key: "line_move_due_at", Value: 1}}).SetLimit(200))
	if err != nil {
		return nil, errInternal("line moves lookup failed")
	}
	out := []referral{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("line moves decode failed")
	}
	return out, nil
}

// listPlannedLineMoves: swaps planned on farmID and never finished.
func (r *repository) listPlannedLineMoves(ctx context.Context, farmID string) ([]referral, error) {
	cur, err := r.referrals().Find(ctx, bson.D{
		{Key: "line_move.farm_id", Value: farmID},
		{Key: "line_move.result", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}, options.Find().SetLimit(50))
	if err != nil {
		return nil, errInternal("line moves lookup failed")
	}
	out := []referral{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("line moves decode failed")
	}
	return out, nil
}

// planReferralLineMove writes the swap before either member moves; false
// when the referral already carries one.
func (r *repository) planReferralLineMove(ctx context.Context, id primitive.ObjectID, plan referralLineMove) (bool, error) {
	res, err := r.referrals().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "line_move", Value: bson.D{{Key: "$exists", Value: false}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "line_move", Value: plan}}}})
	if err != nil {
		return false, errInternal("referral update failed")
	}
	return res.ModifiedCount == 1, nil
}

// finishReferralLineMove stamps the outcome; a plan already finished keeps
// its result.
func (r *repository) finishReferralLineMove(ctx context.Context, id primitive.ObjectID, plan referralLineMove) (bool, error) {
	res, err := r.referrals().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}, {Key: "line_move.result", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}}},
		bson.D{
			{Key: "$set", Value: bson.D{{Key: "line_move", Value: plan}}},
			{Key: "$unset", Value: bson.D{{Key: "line_move_due_at", Value: ""}}},
		})
	if err != nil {
		return false, errInternal("referral update failed")
	}
	return res.ModifiedCount == 1, nil
}

// memberAheadInLine is the waiting member of farmID holding the largest line
// number below line, or nil when line is first.
func (r *repository) memberAheadInLine(ctx context.Context, farmID string, line int) (*foundingMember, error) {
	var m foundingMember
	err := r.foundingMembers().FindOne(ctx,
		bson.D{
			{Key: "farm_id", Value: farmID}, {Key: "status", Value: memberWaiting},
			{Key: "line_number", Value: bson.D{{Key: "$gt", Value: 0}, {Key: "$lt", Value: line}}},
		},
		options.FindOne().SetSort(bson.D{{Key: "line_number", Value: -1}})).Decode(&m)
	if isNoDocs(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errInternal("member lookup failed")
	}
	return &m, nil
}

// takeFarmLineLease holds farmID's line for one swap; false while another
// holder's lease runs.
func (r *repository) takeFarmLineLease(ctx context.Context, farmID, token string, now time.Time) bool {
	res, err := r.foundingFarms().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: farmID}, {Key: "$or", Value: bson.A{
			bson.D{{Key: "line_lock_until", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "line_lock_until", Value: nil}},
			bson.D{{Key: "line_lock_until", Value: bson.D{{Key: "$lte", Value: now}}}},
		}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "line_lock_until", Value: now.Add(farmLineLease)}, {Key: "line_lock_by", Value: token}}}})
	return err == nil && res.ModifiedCount == 1
}

func (r *repository) releaseFarmLineLease(ctx context.Context, farmID, token string) {
	_, _ = r.foundingFarms().UpdateOne(context.WithoutCancel(ctx),
		bson.D{{Key: "_id", Value: farmID}, {Key: "line_lock_by", Value: token}},
		bson.D{{Key: "$unset", Value: bson.D{{Key: "line_lock_until", Value: ""}, {Key: "line_lock_by", Value: ""}}}})
}

// swapLineNumbers carries out a planned swap. Each update is guarded on the
// number the member held before it, so running it again after a partial
// run finishes it and running it after a full one changes nothing.
func (r *repository) swapLineNumbers(ctx context.Context, plan referralLineMove) error {
	if _, err := r.foundingMembers().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: plan.AheadID}, {Key: "farm_id", Value: plan.FarmID}, {Key: "line_number", Value: plan.To}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "line_number", Value: plan.From}, {Key: "updated_at", Value: time.Now().UTC()}}}}); err != nil {
		return errInternal("line swap failed")
	}
	if _, err := r.foundingMembers().UpdateOne(ctx,
		bson.D{{Key: "_id", Value: plan.MemberID}, {Key: "farm_id", Value: plan.FarmID}, {Key: "line_number", Value: plan.From}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "line_number", Value: plan.To}, {Key: "updated_at", Value: time.Now().UTC()}}}}); err != nil {
		return errInternal("line swap failed")
	}
	return nil
}

// ── Service ─────────────────────────────────────────────────────────────────

// referralLineMoveOnJoin runs after a friend's paid Founding Family join:
// the referral earns its line movement (once, ever) and the move is made
// now when it can be; the referral worker retries one that could not.
// Best-effort: nothing here may fail the friend's join.
func (s *service) referralLineMoveOnJoin(ctx context.Context, refereeID primitive.ObjectID, friendFarm string, now time.Time) {
	ref, err := s.repo.markReferralLineMoveDue(ctx, refereeID, friendFarm, now)
	if err != nil || ref == nil {
		return
	}
	s.applyReferralLineMove(ctx, ref.ID, now)
}

// applyDueLineMoves is the referral worker's sweep: every earned line
// movement not finished yet (a busy farm, a restart). Returns how many
// referrers moved up.
func (s *service) applyDueLineMoves(ctx context.Context, now time.Time) int {
	due, err := s.repo.listDueLineMoves(ctx, now)
	if err != nil {
		s.log.WarnContext(ctx, "founding line: due scan failed", "err", err)
		return 0
	}
	moved := 0
	for i := range due {
		if s.applyReferralLineMove(ctx, due[i].ID, now) == lineMoveMoved {
			moved++
		}
	}
	return moved
}

// applyReferralLineMove makes one referral's line movement. Returns the
// result it finished with ("" when it must be retried).
func (s *service) applyReferralLineMove(ctx context.Context, refID primitive.ObjectID, now time.Time) string {
	ref, err := s.repo.findReferralByID(ctx, refID)
	if err != nil || ref == nil {
		return ""
	}
	if ref.LineMove != nil && ref.LineMove.Result != "" {
		return ref.LineMove.Result
	}
	token := "line:" + refID.Hex()
	if ref.LineMove != nil { // planned by a holder that did not finish
		if !s.repo.takeFarmLineLease(ctx, ref.LineMove.FarmID, token, now) {
			return ""
		}
		defer s.repo.releaseFarmLineLease(ctx, ref.LineMove.FarmID, token)
		return s.finishPlannedLineMove(ctx, ref, now)
	}
	m, err := s.repo.findFoundingMember(ctx, ref.ReferrerID)
	if err != nil {
		return ""
	}
	if m == nil || m.Status != memberWaiting || m.LineNumber <= 0 {
		return s.closeLineMove(ctx, ref, lineMoveNotWaiting, now)
	}
	farm, err := s.repo.findFoundingFarm(ctx, m.FarmID)
	if err != nil {
		return ""
	}
	if farm == nil || farm.Status != farmFilling {
		return s.closeLineMove(ctx, ref, lineMoveNotWaiting, now)
	}
	if !s.repo.takeFarmLineLease(ctx, farm.ID, token, now) {
		return "" // another swap holds this farm's line: the worker retries
	}
	defer s.repo.releaseFarmLineLease(ctx, farm.ID, token)
	// Another referral's swap on this farm that stopped half way is finished
	// first, so the numbers read below are whole.
	if planned, perr := s.repo.listPlannedLineMoves(ctx, farm.ID); perr == nil {
		for i := range planned {
			if planned[i].ID != ref.ID {
				s.finishPlannedLineMove(ctx, &planned[i], now)
			}
		}
	}
	// Read again under the lease: the member may have stopped, or the farm
	// unlocked, since.
	if m, err = s.repo.findFoundingMember(ctx, ref.ReferrerID); err != nil {
		return ""
	}
	if m == nil || m.Status != memberWaiting || m.LineNumber <= 0 || m.FarmID != farm.ID {
		return s.closeLineMove(ctx, ref, lineMoveNotWaiting, now)
	}
	ahead, err := s.repo.memberAheadInLine(ctx, farm.ID, m.LineNumber)
	if err != nil {
		return ""
	}
	if ahead == nil {
		return s.closeLineMove(ctx, ref, lineMoveFront, now)
	}
	plan := referralLineMove{FarmID: farm.ID, MemberID: m.ID, From: m.LineNumber, AheadID: ahead.ID, To: ahead.LineNumber, At: now.UTC()}
	if ok, perr := s.repo.planReferralLineMove(ctx, ref.ID, plan); perr != nil || !ok {
		return ""
	}
	ref.LineMove = &plan
	return s.finishPlannedLineMove(ctx, ref, now)
}

// finishPlannedLineMove carries out (or completes) a planned swap, stamps
// the referral moved and tells the referrer. The caller holds the lease.
func (s *service) finishPlannedLineMove(ctx context.Context, ref *referral, now time.Time) string {
	plan := *ref.LineMove
	if err := s.repo.swapLineNumbers(ctx, plan); err != nil {
		s.log.WarnContext(ctx, "founding line: swap failed - the worker finishes it", "referral", ref.ID.Hex(), "err", err)
		return ""
	}
	plan.Result = lineMoveMoved
	plan.At = now.UTC()
	finished, err := s.repo.finishReferralLineMove(ctx, ref.ID, plan)
	if err != nil {
		return ""
	}
	if finished {
		s.tellLineMoved(ctx, ref, plan)
	}
	return lineMoveMoved
}

// closeLineMove records a referral whose friend paid while its referrer was
// not in a line to move up in (or already first). Nothing moves, nobody is
// told; the Rs 100 credit is unaffected.
func (s *service) closeLineMove(ctx context.Context, ref *referral, result string, now time.Time) string {
	if _, err := s.repo.finishReferralLineMove(ctx, ref.ID, referralLineMove{Result: result, At: now.UTC()}); err != nil {
		return ""
	}
	return result
}

// tellLineMoved emits founding.line_moved (FF-04) to the referrer: their new
// place, and the friend's farm with its homes to go.
func (s *service) tellLineMoved(ctx context.Context, ref *referral, plan referralLineMove) {
	payload := map[string]any{
		"referral_id": ref.ID.Hex(), "line": plan.To, "from": plan.From, "referrer_farm_id": plan.FarmID,
		"scope_key": "founding:line:" + ref.ID.Hex(),
	}
	friend, _ := s.repo.findAccountByID(ctx, ref.RefereeID)
	payload["friend"] = lineMoveFriendLabel(friend)
	if f, _ := s.repo.findFoundingFarm(ctx, ref.LineMoveFriendFarm); f != nil {
		togo := f.UnlocksAt - f.Claimed
		if togo < 0 || f.Status == farmUnlocked {
			togo = 0
		}
		payload["farm_id"], payload["farm"], payload["farmer"] = f.ID, f.Name, f.Farmer
		payload["togo"] = togo
		payload["friend_farm_unlocked"] = f.Status == farmUnlocked
	}
	s.emitCRMEvent(ctx, "founding.line_moved", ref.ReferrerID, payload)
}

// lineMoveFriendLabel names the friend in FF-04: their first name, else the
// last four digits of their number. At most 30 characters (one DLT variable).
func lineMoveFriendLabel(a *account) string {
	if a != nil && a.FullName != nil {
		if f := strings.Fields(*a.FullName); len(f) > 0 {
			name := f[0]
			if len([]rune(name)) > 30 {
				name = string([]rune(name)[:30])
			}
			return name
		}
	}
	if a != nil {
		if d := normalizePhone(a.Phone); len(d) >= 4 {
			return "(number ending " + d[len(d)-4:] + ")"
		}
	}
	return "(a new home)"
}
