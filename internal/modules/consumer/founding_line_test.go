package consumer

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// farmLine is the waiting line of a farm: line number -> member.
func farmLine(t *testing.T, w *chainWorld, farmID string) map[int]primitive.ObjectID {
	t.Helper()
	rows, err := w.svc.repo.listFarmMembers(context.Background(), farmID, memberWaiting)
	if err != nil {
		t.Fatalf("line of %s: %v", farmID, err)
	}
	out := map[int]primitive.ObjectID{}
	for _, m := range rows {
		if prev, dup := out[m.LineNumber]; dup {
			t.Fatalf("line #%d of %s is held twice: %s and %s", m.LineNumber, farmID, prev.Hex(), m.ConsumerID.Hex())
		}
		out[m.LineNumber] = m.ConsumerID
	}
	return out
}

func lineOf(t *testing.T, w *chainWorld, cid primitive.ObjectID) int {
	t.Helper()
	m, err := w.svc.repo.findFoundingMember(context.Background(), cid)
	if err != nil || m == nil {
		t.Fatalf("member %s: %v", cid.Hex(), err)
	}
	return m.LineNumber
}

func lineMovedEvents(t *testing.T, w *chainWorld, cid primitive.ObjectID) []crmEvent {
	t.Helper()
	cur, err := w.db.Collection(collCRMEvents).Find(context.Background(),
		bson.D{{Key: "topic", Value: "founding.line_moved"}, {Key: "consumer_id", Value: cid}})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []crmEvent
	if err := cur.All(context.Background(), &out); err != nil {
		t.Fatalf("events decode: %v", err)
	}
	return out
}

// Spec 5.8 with the founder's 25 Sep call: a friend who pays the Rs 99
// moves a WAITING referrer up one place on their own farm's line (a swap
// with the member directly ahead), once per referral; numbers stay unique,
// next_line never moves, an active referrer and one already first are
// unaffected, and the Rs 100 credit is untouched.
func TestReferralJoinMovesTheWaitingReferrerUpTheLine(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 10) // Gonard fills at 10, Mishra at 80
	join := func(cid primitive.ObjectID, farm string) {
		t.Helper()
		if _, err := w.svc.joinFoundingFamily(ctx, cid, farm); err != nil {
			t.Fatalf("join %s: %v", farm, err)
		}
	}
	a := w.customer(t, "9000019101", 300)
	b := w.customer(t, "9000019102", 300)
	r := w.customer(t, "9000019103", 300)
	for _, cid := range []primitive.ObjectID{a, b, r} {
		join(cid, "gonard-dairy")
	}
	if lineOf(t, w, a) != 1 || lineOf(t, w, b) != 2 || lineOf(t, w, r) != 3 {
		t.Fatalf("setup lines: %d %d %d", lineOf(t, w, a), lineOf(t, w, b), lineOf(t, w, r))
	}

	// A friend applies R's code and pays the Rs 99 on another farm: R swaps
	// with B, the member directly ahead. A keeps #1.
	f := w.customer(t, "9000019104", 300)
	if _, err := w.svc.repo.updateAccount(ctx, f, bson.D{{Key: "full_name", Value: "Neha Verma"}}); err != nil {
		t.Fatalf("name: %v", err)
	}
	referFriend(t, w, r, f)
	join(f, "mishra-dairy")
	if lineOf(t, w, r) != 2 || lineOf(t, w, b) != 3 || lineOf(t, w, a) != 1 {
		t.Fatalf("after the friend paid: R #%d B #%d A #%d, want 2, 3, 1", lineOf(t, w, r), lineOf(t, w, b), lineOf(t, w, a))
	}
	if farm, _ := w.svc.repo.findFoundingFarm(ctx, "gonard-dairy"); farm.NextLine != 3 || farm.Claimed != 3 {
		t.Fatalf("a move must not touch the farm's counters: %+v", farm)
	}
	ref, _ := w.svc.repo.findReferralByReferee(ctx, f)
	if ref.LineMove == nil || ref.LineMove.Result != lineMoveMoved || ref.LineMove.From != 3 || ref.LineMove.To != 2 || ref.LineMoveDueAt != nil {
		t.Fatalf("the referral's line move: %+v due %v", ref.LineMove, ref.LineMoveDueAt)
	}
	evs := lineMovedEvents(t, w, r)
	if len(evs) != 1 {
		t.Fatalf("founding.line_moved for R: %d want 1", len(evs))
	}
	p := evs[0].Payload
	togo, _ := crmPayloadNumber(p["togo"])
	newLine, _ := crmPayloadNumber(p["line"])
	if p["friend"] != "Neha" || p["farm"] != "Mishra Dairy" || togo != 79 || newLine != 2 || p["friend_farm_unlocked"] != false {
		t.Fatalf("line_moved payload: %v", p)
	}
	// The Rs 100 credit is a separate track: nothing was credited by a join.
	if rw, _ := w.svc.wallet(ctx, r); rw.Rewards != 0 {
		t.Fatalf("a join paid a referral credit: %v", rw.Rewards)
	}

	// The friend stops (seat released) and pays again: a referral moves its
	// referrer once, ever.
	if _, err := w.svc.stopFoundingFamily(ctx, f); err != nil {
		t.Fatalf("stop: %v", err)
	}
	join(f, "mishra-dairy")
	if w.svc.applyDueLineMoves(ctx, time.Now()) != 0 || lineOf(t, w, r) != 2 || len(lineMovedEvents(t, w, r)) != 1 {
		t.Fatalf("a second join by the same friend moved R again: #%d", lineOf(t, w, r))
	}

	// A second friend claims R's own farm: R moves to the front (swap with
	// A), the friend takes the next number, and the line is still whole.
	g := w.customer(t, "9000019105", 300)
	referFriend(t, w, r, g)
	join(g, "gonard-dairy")
	if lineOf(t, w, r) != 1 || lineOf(t, w, a) != 2 || lineOf(t, w, g) != 4 {
		t.Fatalf("second friend: R #%d A #%d G #%d, want 1, 2, 4", lineOf(t, w, r), lineOf(t, w, a), lineOf(t, w, g))
	}
	line := farmLine(t, w, "gonard-dairy")
	var nums []int
	for n := range line {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	if len(nums) != 4 || nums[0] != 1 || nums[3] != 4 {
		t.Fatalf("gonard line after two moves: %v", nums)
	}

	// Already first: a third friend moves nobody and says nothing.
	h := w.customer(t, "9000019106", 300)
	referFriend(t, w, r, h)
	join(h, "mishra-dairy")
	ref, _ = w.svc.repo.findReferralByReferee(ctx, h)
	if ref.LineMove == nil || ref.LineMove.Result != lineMoveFront || lineOf(t, w, r) != 1 || len(lineMovedEvents(t, w, r)) != 2 {
		t.Fatalf("a referrer already first: %+v #%d", ref.LineMove, lineOf(t, w, r))
	}

	// An active referrer (their farm unlocked) is unaffected.
	if _, err := w.svc.upsertFoundingFarms(ctx, []foundingFarmInput{{ID: "solo-farm", Name: "Solo Farm", Farmer: "Ram", UnlocksAt: 1}}, "test"); err != nil {
		t.Fatalf("solo farm: %v", err)
	}
	act := w.customer(t, "9000019107", 300)
	join(act, "solo-farm")
	before := lineOf(t, w, act)
	k := w.customer(t, "9000019108", 300)
	referFriend(t, w, act, k)
	join(k, "mishra-dairy")
	ref, _ = w.svc.repo.findReferralByReferee(ctx, k)
	if ref.LineMove == nil || ref.LineMove.Result != lineMoveNotWaiting || lineOf(t, w, act) != before || len(lineMovedEvents(t, w, act)) != 0 {
		t.Fatalf("an active referrer: %+v #%d", ref.LineMove, lineOf(t, w, act))
	}
}

// A swap that stopped half way (the process died between the two member
// updates) leaves two members on one number; the next holder of that farm's
// line, or the referral worker, finishes it before anything else moves.
func TestReferralLineMoveFinishesAHalfDoneSwap(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 10)
	a := w.customer(t, "9000019201", 300)
	r := w.customer(t, "9000019202", 300)
	f := w.customer(t, "9000019203", 300)
	for _, cid := range []primitive.ObjectID{a, r} {
		if _, err := w.svc.joinFoundingFamily(ctx, cid, "gonard-dairy"); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	referFriend(t, w, r, f)
	ma, _ := w.svc.repo.findFoundingMember(ctx, a)
	mr, _ := w.svc.repo.findFoundingMember(ctx, r)
	ref, _ := w.svc.repo.findReferralByReferee(ctx, f)
	now := time.Now().UTC()
	// The state a crash leaves: earned, planned, and A already moved to #2.
	if _, err := w.db.Collection(collReferrals).UpdateByID(ctx, ref.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "line_move_due_at", Value: now}, {Key: "line_move_friend_farm", Value: "mishra-dairy"},
		{Key: "line_move", Value: referralLineMove{FarmID: "gonard-dairy", MemberID: mr.ID, From: 2, AheadID: ma.ID, To: 1, At: now}},
	}}}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := w.db.Collection(collFoundingMembers).UpdateByID(ctx, ma.ID, bson.D{{Key: "$set", Value: bson.D{{Key: "line_number", Value: 2}}}}); err != nil {
		t.Fatalf("half swap: %v", err)
	}
	if n := w.svc.applyDueLineMoves(ctx, now.Add(time.Minute)); n != 1 {
		t.Fatalf("the sweep finished %d moves, want 1", n)
	}
	if lineOf(t, w, r) != 1 || lineOf(t, w, a) != 2 {
		t.Fatalf("after the finish: R #%d A #%d", lineOf(t, w, r), lineOf(t, w, a))
	}
	farmLine(t, w, "gonard-dairy") // no number held twice
	if w.svc.applyDueLineMoves(ctx, now.Add(2*time.Minute)) != 0 || lineOf(t, w, r) != 1 {
		t.Fatalf("a second sweep moved again")
	}
}

// Friends of different referrers paying at the same moment on one farm:
// every referrer moves at most once and no number is ever held twice.
func TestReferralLineMovesRacingOnOneFarmKeepNumbersUnique(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	seedTestFarms(t, w, 50)
	const n = 6
	var referrers, friends []primitive.ObjectID
	for i := 0; i < n; i++ {
		rr := w.customer(t, "90000193"+string(rune('0'+i))+"1", 300)
		if _, err := w.svc.joinFoundingFamily(ctx, rr, "gonard-dairy"); err != nil {
			t.Fatalf("join referrer: %v", err)
		}
		ff := w.customer(t, "90000193"+string(rune('0'+i))+"2", 300)
		referFriend(t, w, rr, ff)
		referrers, friends = append(referrers, rr), append(friends, ff)
	}
	var wg sync.WaitGroup
	for _, ff := range friends {
		wg.Add(1)
		go func(cid primitive.ObjectID) {
			defer wg.Done()
			_, _ = w.svc.joinFoundingFamily(ctx, cid, "mishra-dairy")
		}(ff)
	}
	wg.Wait()
	// Whatever a busy lease deferred, the worker finishes.
	w.svc.applyDueLineMoves(ctx, time.Now().Add(time.Minute))
	line := farmLine(t, w, "gonard-dairy")
	if len(line) != n {
		t.Fatalf("gonard holds %d waiting numbers, want %d: %v", len(line), n, line)
	}
	for i := 1; i <= n; i++ {
		if _, ok := line[i]; !ok {
			t.Fatalf("number #%d is missing from the line: %v", i, line)
		}
	}
	moved := 0
	for _, ff := range friends {
		ref, _ := w.svc.repo.findReferralByReferee(ctx, ff)
		if ref.LineMove == nil || ref.LineMove.Result == "" {
			t.Fatalf("a referral left unfinished: %+v", ref.LineMove)
		}
		if ref.LineMove.Result == lineMoveMoved {
			moved++
		}
	}
	if moved != n-1 && moved != n { // the first in line may already be at the front
		t.Fatalf("moves: %d", moved)
	}
}
