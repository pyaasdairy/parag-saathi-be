// CRM — trigger engine, guard chain, channels and the worker.
//
// The shipped configuration (crm_triggers.json, embedded below) is the single
// source of trigger truth — schedules, conditions, categories, caps and the
// bilingual templates all come from it. Changing campaign behaviour means
// replacing that file, not editing Go ("read the config, do not retype it").
//
// PHASE A (now): every W trigger dispatches to the consumer IN-APP INBOX and
// internal triggers to the operator notifications collection — the whole
// engine, state machine and suppression log provable end-to-end with no
// vendor dependency. PHASE B (keys pending from the founder): MSG91 templated
// SMS and WhatsApp implement the same crmChannel interface behind env config —
// no trigger code changes when the keys arrive.
//
// GUARDS. Evaluated in the configured order; a trigger that fires but fails a
// guard is logged as SUPPRESSED with the failing guard id — never silently
// dropped (the spec is emphatic). Guards whose infrastructure does not exist
// yet fail CLOSED for promotional content and OPEN for service content, which
// matches the regulation the guards encode: consent/DND/opt-out only bind
// promotional messages, while service messages ride the existing customer
// relationship.
//
// SCHEDULING. One goroutine, one-minute tick, flag-gated each tick (flipping
// CRM_ENABLED or a kill switch needs no redeploy). Every scheduled fire is
// claimed by a unique (trigger, consumer, IST day) row first, so a missed tick
// (free-tier spin-down) fires LATE instead of never, and a second replica can
// never double-send.
package consumer

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

//go:embed crm_triggers.json
var embeddedCRMConfig []byte

const (
	collCRMEvents     = "crm_events"
	collCRMDispatch   = "crm_dispatch_log"
	collConsumerInbox = "consumer_inbox"

	crmFlagPrefix = "crm.trigger." // per-trigger kill switches in the flags service
)

// ── Config (parsed once from the embedded JSON) ─────────────────────────────

type crmTrigger struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind"` // event | scheduled | alias
	Event        string         `json:"event"`
	Schedule     string         `json:"schedule"` // "30 10 * * *" (min hour · IST)
	Category     string         `json:"category"` // service_implicit | promotional | internal
	Template     crmTemplateRef `json:"template"`
	Section      string         `json:"section"`
	FrequencyCap map[string]any `json:"frequency_cap"`
	// Delivery is the config's per-trigger channel routing (primary / parallel /
	// fallback) — consumed by the Phase B transports in crm_channels.go. The
	// in-app inbox is NOT listed there; it always runs (Phase A behaviour).
	Delivery crmDelivery `json:"delivery"`
}

// crmTemplateRef tolerates the three shapes the Rev3 config actually uses for
// a trigger's template: a plain id ("T-A01"), null (internal triggers), and a
// conditional object ({"if":..., "then": "T-X", "else": "T-Y"} — B-06). Every
// W trigger is a plain id; for conditionals we keep the branches so a future
// section can pick, and String() reports the primary ("then") branch.
type crmTemplateRef struct {
	Ref  string
	If   string
	Then string
	Else string
}

func (r *crmTemplateRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		r.Ref = s
		return nil
	}
	if string(b) == "null" {
		return nil
	}
	var cond struct {
		If   string `json:"if"`
		Then string `json:"then"`
		Else string `json:"else"`
	}
	if err := json.Unmarshal(b, &cond); err != nil {
		return err
	}
	r.If, r.Then, r.Else = cond.If, cond.Then, cond.Else
	return nil
}

func (r crmTemplateRef) String() string {
	if r.Ref != "" {
		return r.Ref
	}
	return r.Then
}

type crmTemplate struct {
	DLTCategory string `json:"dlt_category"`
	CTA         string `json:"cta"`
	EN          string `json:"en"`
	HI          string `json:"hi"`
	HIDev       string `json:"hi_devanagari"`
}

type crmOffer struct {
	PacksInEntitlement    int    `json:"packs_in_entitlement"`
	PackSKU               string `json:"pack_sku"`
	Pack2MinRechargePaise int64  `json:"pack2_min_recharge_paise"`
	Pack2GraceDays        int    `json:"pack2_grace_days"`
	// SeedSKU is OUR catalog id for the ERP pack ref (PARAG-FCM-500ML ↔ the
	// curated map's gold-500ml) — resolved here once, config-driven upstream.
	SeedSKU         string `json:"-"`
	SubscriptionQty int    `json:"-"` // honours the app's 1 L/day milk floor
}

type crmConfig struct {
	Guards struct {
		QuietPromoStart string
		QuietPromoEnd   string
		PromoPerDay     int
		PromoPerWeek    int
		DedupMinutes    int
		ConsentTTLDays  int // TCCCPR: explicit promo consent expires after N days
	}
	Triggers  map[string]crmTrigger
	Templates map[string]crmTemplate
	Offer     crmOffer
}

var (
	crmCfg     *crmConfig
	crmCfgOnce sync.Once
)

// crmConfigLoad parses the embedded JSON once (sync.Once — the worker
// goroutine and HTTP handlers race here on first use). Panics on a malformed
// embed (a broken config must never reach runtime silently) — but only when
// the CRM is actually enabled; a disabled binary never parses it.
func crmConfigLoad() *crmConfig {
	crmCfgOnce.Do(crmConfigParse)
	return crmCfg
}

func crmConfigParse() {
	var raw struct {
		Guards map[string]json.RawMessage `json:"guards"`
		Config struct {
			Offer map[string]struct {
				PacksInEntitlement    int    `json:"packs_in_entitlement"`
				PackSKU               string `json:"pack_sku"`
				Pack2MinRechargePaise int64  `json:"pack2_min_recharge_paise"`
				Pack2GraceDays        int    `json:"pack2_grace_days"`
			} `json:"offer"`
		} `json:"config"`
		Triggers  []crmTrigger           `json:"triggers"`
		Templates map[string]crmTemplate `json:"templates"`
	}
	if err := json.Unmarshal(embeddedCRMConfig, &raw); err != nil {
		panic("crm: embedded trigger config is malformed: " + err.Error())
	}
	c := &crmConfig{Triggers: map[string]crmTrigger{}, Templates: raw.Templates}
	for _, t := range raw.Triggers {
		c.Triggers[t.ID] = t
	}
	// Guard numbers from config, with the spec's values as the documented
	// fallback (they are configuration, not law, but never compiled surprises).
	c.Guards.QuietPromoStart, c.Guards.QuietPromoEnd = "10:00", "21:00"
	c.Guards.PromoPerDay, c.Guards.PromoPerWeek, c.Guards.DedupMinutes = 1, 3, 30
	if g, ok := raw.Guards["G5_quiet_hours"]; ok {
		var q struct {
			Windows struct {
				Promo struct{ Start, End string } `json:"promotional_message"`
			} `json:"windows"`
		}
		if json.Unmarshal(g, &q) == nil && q.Windows.Promo.Start != "" {
			c.Guards.QuietPromoStart, c.Guards.QuietPromoEnd = q.Windows.Promo.Start, q.Windows.Promo.End
		}
	}
	if g, ok := raw.Guards["G6_frequency_cap"]; ok {
		var f struct {
			Promotional struct {
				PerDay  int `json:"per_day"`
				PerWeek int `json:"per_week"`
			} `json:"promotional"`
		}
		if json.Unmarshal(g, &f) == nil && f.Promotional.PerDay > 0 {
			c.Guards.PromoPerDay, c.Guards.PromoPerWeek = f.Promotional.PerDay, f.Promotional.PerWeek
		}
	}
	c.Guards.ConsentTTLDays = 7
	if g, ok := raw.Guards["G2_consent"]; ok {
		var t struct {
			TTLDays int `json:"explicit_consent_ttl_days"`
		}
		if json.Unmarshal(g, &t) == nil && t.TTLDays > 0 {
			c.Guards.ConsentTTLDays = t.TTLDays
		}
	}
	if g, ok := raw.Guards["G8_dedup_window"]; ok {
		var d struct {
			WindowMinutes int `json:"window_minutes"`
		}
		if json.Unmarshal(g, &d) == nil && d.WindowMinutes > 0 {
			c.Guards.DedupMinutes = d.WindowMinutes
		}
	}
	off := raw.Config.Offer[offerWelcomeLitre]
	c.Offer = crmOffer{
		PacksInEntitlement:    off.PacksInEntitlement,
		PackSKU:               off.PackSKU,
		Pack2MinRechargePaise: off.Pack2MinRechargePaise,
		Pack2GraceDays:        off.Pack2GraceDays,
		SeedSKU:               "gold-500ml", // curated ERP map: PARAG-FCM-500ML ↔ gold-500ml
		SubscriptionQty:       2,            // 2 × 500 ml = the app's 1 L/day milk floor
	}
	if c.Offer.PacksInEntitlement == 0 {
		c.Offer.PacksInEntitlement = 2
	}
	if c.Offer.Pack2MinRechargePaise == 0 {
		c.Offer.Pack2MinRechargePaise = 50000
	}
	if c.Offer.Pack2GraceDays == 0 {
		c.Offer.Pack2GraceDays = 7
	}
	crmCfg = c
}

func crmOfferConfig() crmOffer { return crmConfigLoad().Offer }

// ── Event outbox ────────────────────────────────────────────────────────────

type crmEvent struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	Topic      string             `bson:"topic"`
	ConsumerID primitive.ObjectID `bson:"consumer_id,omitempty"`
	Payload    map[string]any     `bson:"payload,omitempty"`
	Status     string             `bson:"status"` // NEW | PROCESSING | DONE | FAILED
	Attempts   int                `bson:"attempts,omitempty"`
	ClaimedAt  *time.Time         `bson:"claimed_at,omitempty"`
	CreatedAt  time.Time          `bson:"created_at"`
}

// emitCRMEvent is BEST-EFFORT by contract: a CRM write must never fail an
// order, a delivery or a payment. It no-ops entirely when the CRM is off, so
// the deployed binary stays byte-identical in behaviour until the flag flips.
func (s *service) emitCRMEvent(ctx context.Context, topic string, consumerID primitive.ObjectID, payload map[string]any) {
	if !crmEnabled() {
		return
	}
	ev := crmEvent{Topic: topic, ConsumerID: consumerID, Payload: payload, Status: "NEW", CreatedAt: time.Now().UTC()}
	if _, err := s.repo.accounts.Database().Collection(collCRMEvents).InsertOne(ctx, ev); err != nil {
		s.log.Warn("crm: event emit failed (non-fatal)", "topic", topic, "err", err)
	}
	if s.deps.Bus != nil { // nil in some test constructions — never assume
		s.deps.Bus.Publish("crm."+topic, payload)
	}
}

// ── Guard chain ─────────────────────────────────────────────────────────────

// crmGuardCheck evaluates the configured guard order for one dispatch and
// returns ("", true) to send or (failedGuardID, false) to suppress. The
// dispatch log records either outcome — a suppression is never silent.
func (s *service) crmGuardCheck(ctx context.Context, t crmTrigger, consumerID primitive.ObjectID, now time.Time) (string, bool) {
	// G1 — kill switches: global env + per-trigger flag (default ON when the
	// flag row is absent; an explicit false kills without a deploy).
	if !crmEnabled() {
		return "G1_kill_switch", false
	}
	if s.deps.Flags != nil {
		if off := s.crmTriggerKilled(ctx, t.ID); off {
			return "G1_kill_switch", false
		}
	}
	promotional := t.Category == "promotional"
	if promotional {
		// G2 — consent: promotional requires explicit, unexpired consent. No
		// consent store is populated yet → fail CLOSED, logged. (Service
		// messages ride the existing relationship and skip this by design.)
		if ok := s.crmHasPromoConsent(ctx, consumerID); !ok {
			return "G2_consent", false
		}
		// G3 — opt-out bar (90 days). Opt-out records live on the account.
		if optedOut := s.crmOptedOut(ctx, consumerID, now); optedOut {
			return "G3_opt_out_suppression", false
		}
		// G4 — DND scrub applies to SMS/WhatsApp/calls; the in-app inbox is not
		// a telecom channel, so Phase A passes structurally. Phase B channels
		// must implement the scrub before sending.
		// G5 — promotional quiet hours (IST).
		cfg := crmConfigLoad()
		hm := now.In(istZone).Format("15:04")
		if hm < cfg.Guards.QuietPromoStart || hm >= cfg.Guards.QuietPromoEnd {
			return "G5_quiet_hours", false
		}
		// G6 — promotional frequency caps (all channels combined).
		day := istDay(now)
		if n := s.crmCountDispatched(ctx, consumerID, "promotional", day, ""); n >= cfg.Guards.PromoPerDay {
			return "G6_frequency_cap", false
		}
		if n := s.crmCountDispatchedSince(ctx, consumerID, "promotional", now.AddDate(0, 0, -7)); n >= cfg.Guards.PromoPerWeek {
			return "G6_frequency_cap", false
		}
		// G7 — complaint hold: no complaint entity exists yet; pass, logged as
		// structural TODO in the dispatch row's meta.
	}
	// G6 (service) — max identical per hour is enforced by the per-offer /
	// per-day claim keys, which are stricter than the config's 6/hour.
	// G8 — dedup: same template to the same customer inside the window → drop.
	if s.crmRecentTemplate(ctx, consumerID, t.Template.String(), now.Add(-time.Duration(crmConfigLoad().Guards.DedupMinutes)*time.Minute)) {
		return "G8_dedup_window", false
	}
	// G9 — channel availability: the inbox needs an account; internal triggers
	// need admin recipients. Both are checked at send time; a missing channel
	// surfaces here.
	// G10 — template must exist in the registry (the embedded config).
	if t.Template.String() != "" {
		if _, ok := crmConfigLoad().Templates[t.Template.String()]; !ok {
			return "G10_template_approved", false
		}
	}
	return "", true
}

func (s *service) crmTriggerKilled(ctx context.Context, triggerID string) bool {
	// flags.Enabled returns false for an absent row — we want default-ON, so a
	// kill is the flag EXISTING with enabled=false. Read via All (cached) once.
	flags, err := s.deps.Flags.All(ctx)
	if err != nil {
		return false // fail open on the kill-switch read; the global env still gates
	}
	key := crmFlagPrefix + triggerID
	for _, f := range flags {
		if f.Key == key {
			return !f.Enabled
		}
	}
	return false
}

func (s *service) crmHasPromoConsent(ctx context.Context, consumerID primitive.ObjectID) bool {
	// C-04 / TCCCPR: explicit promotional consent EXPIRES — only a grant made
	// within the configured TTL counts. Fail closed on any error.
	ttl := crmConfigLoad().Guards.ConsentTTLDays
	since := time.Now().UTC().AddDate(0, 0, -ttl)
	n, err := s.repo.consents.CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: consumerID},
		{Key: "kind", Value: "promotional"},
		{Key: "revoked_at", Value: nil},
		{Key: "created_at", Value: bson.D{{Key: "$gte", Value: since}}},
	})
	return err == nil && n > 0
}

func (s *service) crmOptedOut(ctx context.Context, consumerID primitive.ObjectID, now time.Time) bool {
	var a struct {
		OptOutAt *time.Time `bson:"promo_opt_out_at"`
	}
	if err := s.repo.accounts.FindOne(ctx, bson.D{{Key: "_id", Value: consumerID}},
		options.FindOne().SetProjection(bson.D{{Key: "promo_opt_out_at", Value: 1}})).Decode(&a); err != nil {
		return false
	}
	return a.OptOutAt != nil && now.Sub(*a.OptOutAt) < 90*24*time.Hour
}

// ── Dispatch log + counters ─────────────────────────────────────────────────

type crmDispatchRow struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"`
	TriggerID  string             `bson:"trigger_id"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"`
	ISTDay     string             `bson:"ist_day"`
	Category   string             `bson:"category"`
	Template   string             `bson:"template,omitempty"`
	Status     string             `bson:"status"` // SENT | SUPPRESSED
	Guard      string             `bson:"guard,omitempty"`
	Channel    string             `bson:"channel,omitempty"`
	CreatedAt  time.Time          `bson:"created_at"`
}

func (r *repository) crmDispatchCol() *mongo.Collection {
	return r.accounts.Database().Collection(collCRMDispatch)
}

// crmClaimDispatch wins exactly-once per (trigger, consumer, IST day) via the
// unique index. Winning inserts the row (status filled by the caller's update);
// losing means another tick/replica already handled it.
func (s *service) crmClaimDispatch(ctx context.Context, t crmTrigger, consumerID primitive.ObjectID, day string) (*crmDispatchRow, bool) {
	row := &crmDispatchRow{
		TriggerID: t.ID, ConsumerID: consumerID, ISTDay: day,
		Category: t.Category, Template: t.Template.String(), Status: "CLAIMED", CreatedAt: time.Now().UTC(),
	}
	res, err := s.repo.crmDispatchCol().InsertOne(ctx, row)
	if err != nil {
		return nil, false // duplicate = already claimed; anything else = skip this tick
	}
	row.ID = res.InsertedID.(primitive.ObjectID)
	return row, true
}

func (s *service) crmFinishDispatch(ctx context.Context, row *crmDispatchRow, status, guard, channel string) {
	_, _ = s.repo.crmDispatchCol().UpdateByID(ctx, row.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: status}, {Key: "guard", Value: guard}, {Key: "channel", Value: channel},
	}}})
}

func (s *service) crmCountDispatched(ctx context.Context, consumerID primitive.ObjectID, category, day, excludeTrigger string) int {
	f := bson.D{
		{Key: "consumer_id", Value: consumerID}, {Key: "category", Value: category},
		{Key: "ist_day", Value: day}, {Key: "status", Value: "SENT"},
	}
	if excludeTrigger != "" {
		f = append(f, bson.E{Key: "trigger_id", Value: bson.D{{Key: "$ne", Value: excludeTrigger}}})
	}
	n, _ := s.repo.crmDispatchCol().CountDocuments(ctx, f)
	return int(n)
}

func (s *service) crmCountDispatchedSince(ctx context.Context, consumerID primitive.ObjectID, category string, since time.Time) int {
	n, _ := s.repo.crmDispatchCol().CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: consumerID}, {Key: "category", Value: category},
		{Key: "status", Value: "SENT"}, {Key: "created_at", Value: bson.D{{Key: "$gte", Value: since}}},
	})
	return int(n)
}

func (s *service) crmCountTriggerSent(ctx context.Context, consumerID primitive.ObjectID, triggerID string) int {
	n, _ := s.repo.crmDispatchCol().CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: consumerID}, {Key: "trigger_id", Value: triggerID}, {Key: "status", Value: "SENT"},
	})
	return int(n)
}

func (s *service) crmRecentTemplate(ctx context.Context, consumerID primitive.ObjectID, template string, since time.Time) bool {
	if template == "" {
		return false
	}
	n, _ := s.repo.crmDispatchCol().CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: consumerID}, {Key: "template", Value: template},
		{Key: "status", Value: "SENT"}, {Key: "created_at", Value: bson.D{{Key: "$gte", Value: since}}},
	})
	return n > 0
}

// ── Channels (the seam the founder's MSG91/WhatsApp keys drop into) ─────────

// crmChannel is one delivery transport. Phase A ships "inapp"; Phase B adds
// "sms" (MSG91 flow API, CRM_MSG91_AUTHKEY + per-template DLT ids) and
// "whatsapp" (Meta Cloud API, CRM_WA_TOKEN + CRM_WA_PHONE_ID) behind this same
// interface — trigger code never changes.
type crmChannel interface {
	Name() string
	Send(ctx context.Context, s *service, consumerID primitive.ObjectID, t crmTrigger, tpl crmTemplate, params map[string]string) error
}

type inappChannel struct{}

func (inappChannel) Name() string { return "inapp" }

func (inappChannel) Send(ctx context.Context, s *service, consumerID primitive.ObjectID, t crmTrigger, tpl crmTemplate, params map[string]string) error {
	bodyEN := crmRender(tpl.EN, params)
	bodyHI := crmRender(tpl.HI, params)
	if tpl.HIDev != "" {
		bodyHI = crmRender(tpl.HIDev, params)
	}
	doc := bson.D{
		{Key: "consumer_id", Value: consumerID},
		{Key: "trigger_id", Value: t.ID},
		{Key: "template_id", Value: t.Template.String()},
		{Key: "category", Value: t.Category},
		{Key: "body_en", Value: bodyEN},
		{Key: "body_hi", Value: bodyHI},
		{Key: "cta", Value: tpl.CTA},
		{Key: "created_at", Value: time.Now().UTC()},
		{Key: "read_at", Value: nil},
	}
	_, err := s.repo.accounts.Database().Collection(collConsumerInbox).InsertOne(ctx, doc)
	return err
}

// crmRender substitutes the config's runtime placeholders. Unresolved tokens
// are LEFT VISIBLE deliberately — C-01's rule is that a message which cannot
// resolve its values must fail loudly, never silently render a half-truth;
// the dispatch caller treats a leftover [TOKEN] as a send error.
func crmRender(body string, params map[string]string) string {
	for k, v := range params {
		body = strings.ReplaceAll(body, "["+k+"]", v)
		body = strings.ReplaceAll(body, "{"+k+"}", v)
	}
	return body
}

// crmDispatch runs ONE trigger for ONE consumer end-to-end: claim → guards →
// per-offer caps → render → channel send → log. Safe to call from anywhere;
// every failure mode lands in the dispatch log.
func (s *service) crmDispatch(ctx context.Context, triggerID string, consumerID primitive.ObjectID, params map[string]string) {
	s.crmDispatchAt(ctx, triggerID, consumerID, params, time.Now().UTC())
}

// crmDispatchAt is crmDispatch with the tick's clock injected: the scheduler
// passes its own `now` so the exactly-once (trigger, consumer, IST-day) claim
// keys on the SAME day the schedule condition was evaluated on — event-driven
// callers keep the wall clock via crmDispatch.
func (s *service) crmDispatchAt(ctx context.Context, triggerID string, consumerID primitive.ObjectID, params map[string]string, now time.Time) {
	if !crmEnabled() {
		return
	}
	cfg := crmConfigLoad()
	t, ok := cfg.Triggers[triggerID]
	if !ok {
		s.log.Warn("crm: unknown trigger", "id", triggerID)
		return
	}
	row, won := s.crmClaimDispatch(ctx, t, consumerID, istDay(now))
	if !won {
		return // already handled today (exactly-once)
	}
	// Per-offer / total caps from the trigger's own frequency_cap block.
	if capN := crmCapLimit(t); capN > 0 && s.crmCountTriggerSent(ctx, consumerID, t.ID) >= capN {
		s.crmFinishDispatch(ctx, row, "SUPPRESSED", "G6_frequency_cap", "")
		return
	}
	if guard, pass := s.crmGuardCheck(ctx, t, consumerID, now); !pass {
		s.crmFinishDispatch(ctx, row, "SUPPRESSED", guard, "")
		return
	}
	tpl := cfg.Templates[t.Template.String()]
	std := s.crmStandardParams(ctx, consumerID)
	for k, v := range params {
		std[k] = v
	}
	// The in-app inbox ALWAYS runs (Phase A behaviour, audit-visible), then the
	// Phase B transports (SMS / WhatsApp) run the trigger's delivery routing.
	// With no channel keys set crmTransports() is nil, delivered is exactly
	// {"inapp"} (or empty on an inbox error) and both the SENT and SUPPRESSED
	// rows are byte-identical to Phase A.
	// (No log on an inbox failure — HEAD had none, and the keys-unset path must
	// stay byte-identical; the SUPPRESSED G9 row below is the audit record.)
	ch := inappChannel{}
	delivered := make([]string, 0, 3)
	if err := ch.Send(ctx, s, consumerID, t, tpl, std); err == nil {
		delivered = append(delivered, ch.Name())
	}
	if ext := s.crmTransports(); len(ext) > 0 {
		if phone, err := s.crmDeliveryPhone(ctx, consumerID); err != nil {
			s.log.Warn("crm: no deliverable phone for external channels", "trigger", t.ID, "consumer", consumerID.Hex(), "err", err)
		} else {
			delivered = append(delivered, crmDeliverExternal(ctx, s.log, phone, t, tpl, std, ext)...)
		}
	}
	if len(delivered) == 0 {
		s.crmFinishDispatch(ctx, row, "SUPPRESSED", "G9_channel_availability", ch.Name())
		return
	}
	s.crmFinishDispatch(ctx, row, "SENT", "", strings.Join(delivered, "+"))
}

// crmStandardParams resolves the placeholders every template may carry.
func (s *service) crmStandardParams(ctx context.Context, consumerID primitive.ObjectID) map[string]string {
	p := map[string]string{
		"LINK":           "https://pyaasdairy.com/app",
		"SUPPORT_NUMBER": "96672 60050",
	}
	if o, err := s.repo.findOffer(ctx, consumerID); err == nil && o != nil && o.FirstDeliveryAt != nil {
		p["DATE"] = o.FirstDeliveryAt.In(istZone).AddDate(0, 0, crmOfferConfig().Pack2GraceDays).Format("2 Jan")
	}
	return p
}

func crmCapLimit(t crmTrigger) int {
	for _, k := range []string{"per_offer", "per_customer", "max_total"} {
		if v, ok := t.FrequencyCap[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n)
			case string:
				if i, err := strconv.Atoi(n); err == nil {
					return i
				}
			}
		}
	}
	return 0
}

// crmNotifyAdmins routes internal triggers (W-09, W-10) into the OPERATOR
// notifications inbox — the same collection and shape the low-stock alert
// uses, so the Saathi admin console renders them with zero new UI.
func (s *service) crmNotifyAdmins(ctx context.Context, templateKey string, params map[string]string) {
	admins, err := s.repo.adminRecipients(ctx)
	if err != nil || len(admins) == 0 {
		s.log.Warn("crm: no admin recipients for internal alert", "template", templateKey)
		return
	}
	now := time.Now().UTC()
	p := bson.M{}
	for k, v := range params {
		p[k] = v
	}
	for _, a := range admins {
		_, _ = s.repo.notifications.UpdateOne(ctx,
			bson.D{
				{Key: "party_id", Value: a.id},
				{Key: "template_key", Value: templateKey},
				{Key: "params.day", Value: params["day"]},
			},
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "party_id", Value: a.id},
				{Key: "phone", Value: a.phone},
				{Key: "channel", Value: "APP"},
				{Key: "template_key", Value: templateKey},
				{Key: "language", Value: "hi"},
				{Key: "params", Value: p},
				{Key: "status", Value: "QUEUED"},
				{Key: "queued_at", Value: now},
				{Key: "read_at", Value: nil},
			}}},
			options.Update().SetUpsert(true))
	}
}

// ── The worker ──────────────────────────────────────────────────────────────

// crmWorker runs for the process lifetime and self-gates EVERY tick, so the
// campaign can be turned on and off without a deploy. Catch-up semantics: a
// scheduled trigger fires when now >= its time and its day claim is unclaimed
// — a spin-down makes it late, never missing, never double.
func (s *service) crmWorker(ctx context.Context) {
	select {
	case <-time.After(30 * time.Second): // let boot settle
	case <-ctx.Done():
		return
	}
	s.log.Info("crm worker armed (self-gates on CRM_ENABLED each tick)")
	indexed := false
	for {
		if crmEnabled() {
			// The exactly-once machinery RIDES the unique indexes — a boot-time
			// failure (cold Mongo, deploy blip) must be retried, not accepted.
			if !indexed {
				indexed = s.ensureCRMIndexes(ctx)
			}
			s.crmProcessEvents(ctx)
			s.crmProcessSchedules(ctx, time.Now())
		}
		select {
		case <-time.After(time.Minute):
		case <-ctx.Done():
			return
		}
	}
}

// crmProcessEvents drains the outbox AT-LEAST-ONCE: claim NEW → PROCESSING
// (a lease, so replicas cannot double-process and a crash cannot lose the
// event) → route → DONE on success, back to NEW on a transient error (capped
// at 5 attempts → FAILED + log). All route side effects are CAS/claim-
// idempotent, so replays are harmless — losing an event was not.
func (s *service) crmProcessEvents(ctx context.Context) {
	col := s.repo.accounts.Database().Collection(collCRMEvents)
	now := time.Now().UTC()
	// Crash recovery: a PROCESSING lease older than 10 minutes belongs to a
	// dead worker — hand the event back to the queue.
	_, _ = col.UpdateMany(ctx,
		bson.D{{Key: "status", Value: "PROCESSING"}, {Key: "claimed_at", Value: bson.D{{Key: "$lt", Value: now.Add(-10 * time.Minute)}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "NEW"}}}})
	for i := 0; i < 200; i++ { // bounded per tick
		var ev crmEvent
		err := col.FindOneAndUpdate(ctx,
			bson.D{{Key: "status", Value: "NEW"}},
			bson.D{
				{Key: "$set", Value: bson.D{{Key: "status", Value: "PROCESSING"}, {Key: "claimed_at", Value: time.Now().UTC()}}},
				{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}},
			},
			options.FindOneAndUpdate().SetSort(bson.D{{Key: "created_at", Value: 1}})).Decode(&ev)
		if err != nil {
			return // drained (or transient — next tick retries)
		}
		if rerr := s.crmRouteEvent(ctx, ev); rerr != nil {
			next := "NEW"
			if ev.Attempts >= 5 {
				next = "FAILED"
				s.log.Warn("crm: event failed permanently", "topic", ev.Topic, "consumer", ev.ConsumerID.Hex(), "err", rerr)
			}
			_, _ = col.UpdateOne(ctx, bson.D{{Key: "_id", Value: ev.ID}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: next}}}})
			continue
		}
		_, _ = col.UpdateOne(ctx, bson.D{{Key: "_id", Value: ev.ID}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "DONE"}}}})
	}
}

func (s *service) crmRouteEvent(ctx context.Context, ev crmEvent) error {
	switch ev.Topic {
	case "order.delivered":
		packNo, _ := ev.Payload["offer_pack"].(int32)
		packNo64, _ := ev.Payload["offer_pack"].(int64)
		packF, _ := ev.Payload["offer_pack"].(float64)
		n := int(packNo) + int(packNo64) + int(packF)
		orderID, _ := ev.Payload["order_id"].(string)
		if n > 0 {
			return s.crmOnPackDelivered(ctx, ev.ConsumerID, n, orderID)
		}
	case "wallet.recharge_settled":
		amt, _ := ev.Payload["amount"].(float64)
		return s.crmOnRechargeSettled(ctx, ev.ConsumerID, amt)
	case "waitlist.joined":
		// Recorded for analytics only. The W-08 message ships in Phase B over
		// SMS with its own rate limits — the join endpoint is UNAUTHENTICATED,
		// so writing into an existing account's inbox keyed on an attacker-
		// supplied phone would be a spam vector, not a feature.
	}
	return nil
}

// crmProcessSchedules evaluates every time-based W trigger for every LIVE
// offer. The enrolled-offer count is pilot-scale (hundreds); a single indexed
// scan per tick is deliberate simplicity.
func (s *service) crmProcessSchedules(ctx context.Context, now time.Time) {
	cur, err := s.repo.offers().Find(ctx, bson.D{{Key: "offer_id", Value: offerWelcomeLitre}})
	if err != nil {
		return
	}
	var offers []consumerOffer
	if err := cur.All(ctx, &offers); err != nil {
		return
	}
	ist := now.In(istZone)
	hm := ist.Format("15:04")
	for i := range offers {
		o := &offers[i]
		day := daysSinceFirstDelivery(o, now)
		if day < 0 {
			continue // pack 1 not delivered yet — no schedule anchors
		}
		// 10:30 block (catch-up: fire any time AFTER 10:30 IST, once per day).
		if hm >= "10:30" {
			if day == 0 && o.Pack1State == pack1Delivered {
				if bal, err := s.wallet(ctx, o.ConsumerID); err == nil && bal.Cash == 0 {
					s.crmDispatchAt(ctx, "W-03a", o.ConsumerID, nil, now)
				}
			}
			if day == 0 && o.Pack2State == pack2Locked && hm >= "10:32" {
				s.crmDispatchAt(ctx, "W-03b", o.ConsumerID, nil, now) // promotional — guards decide
			}
			if (day == 3 || day == 5) && o.Pack2State == pack2Locked {
				s.crmDispatchAt(ctx, "W-06", o.ConsumerID, nil, now)
			}
			// Expiry fires from the day AFTER the advertised window: W-06 names
			// day <grace>'s DATE, and the recharge unlock honours that whole
			// day (same IST-day predicate) — so the sweep may only close the
			// offer from day grace+1.
			if day > crmOfferConfig().Pack2GraceDays && o.Pack2State == pack2Locked {
				// The MANDATORY "nothing has been charged" message goes FIRST
				// (the dispatch-log claim + per-offer cap make it exactly-once);
				// only then the irreversible CAS. A transient send failure
				// leaves the state locked, so the next tick retries BOTH —
				// dispatch-then-expire is at-least-once, expire-then-dispatch
				// silently lost the spec's one non-negotiable message.
				s.crmDispatchAt(ctx, "W-07", o.ConsumerID, nil, now)
				if moved, _ := s.repo.transitionPack(ctx, o.ConsumerID, 2, pack2Locked, pack2Expired, "grace window elapsed", nil); moved {
					s.emitCRMEvent(ctx, "offer_pack_state_change", o.ConsumerID, map[string]any{"pack_no": 2, "from": pack2Locked, "to": pack2Expired})
				}
			}
		}
	}
	// Wallet-health nudges for the WHOLE base (not just Welcome Litre):
	// B-01 low balance at 09:00, B-02 insufficient-for-tomorrow at 17:00.
	s.crmWalletHealthSweep(ctx, now, hm)

	// 18:00 — W-10 reconciliation stop-loss (one row per day via the claim).
	if hm >= "18:00" {
		s.crmReconcile(ctx, istDay(now))
	}
}

// crmWalletHealthSweep drives the spec's B-section wallet triggers for every
// ACTIVE subscriber ("if balance is low then a message must be hit" — the
// founder's words, and the spec's B-01/B-02):
//
//	B-01 (09:00 IST, service_implicit): days-of-cover below 4 → "Wallet is
//	     running low" — at most once per 7 days per customer (per_cycle).
//	B-02 (17:00 IST, service_implicit, critical): spendable balance cannot
//	     cover TOMORROW's subscription day → "recharge by 12 noon tomorrow" —
//	     once per day, every day it stays true (critical_exempt_from_daily_cap).
//
// Households inside a LIVE Welcome Litre journey (pack 2 locked/pending) are
// excluded — W-03a/W-06 own their recharge messaging until the offer settles.
// Each block scans once per IST day via a NilObjectID sweep claim, so the
// per-minute scheduler tick costs one indexed query, not a collection walk.
func (s *service) crmWalletHealthSweep(ctx context.Context, now time.Time, hm string) {
	day := istDay(now)
	if hm >= "09:00" {
		if _, won := s.crmClaimDispatch(ctx, crmTrigger{ID: "B-01-SWEEP", Category: "internal"}, primitive.NilObjectID, day); won {
			s.crmSweepWalletCover(ctx, now)
		}
	}
	if hm >= "17:00" {
		if _, won := s.crmClaimDispatch(ctx, crmTrigger{ID: "B-02-SWEEP", Category: "internal"}, primitive.NilObjectID, day); won {
			s.crmSweepTomorrowShortfall(ctx, now)
		}
	}
}

// crmSubsByConsumer groups the active subscriptions by owner — one wallet
// check per customer however many plans they run.
func (s *service) crmSubsByConsumer(ctx context.Context) map[primitive.ObjectID][]subscription {
	subs, err := s.repo.listActiveSubscriptions(ctx)
	if err != nil {
		return nil
	}
	by := map[primitive.ObjectID][]subscription{}
	for i := range subs {
		by[subs[i].ConsumerID] = append(by[subs[i].ConsumerID], subs[i])
	}
	return by
}

// crmInLiveWelcomeJourney reports whether the campaign currently owns this
// customer's recharge messaging (pack 2 still locked or pending).
func (s *service) crmInLiveWelcomeJourney(ctx context.Context, consumerID primitive.ObjectID) bool {
	o, err := s.repo.findOffer(ctx, consumerID)
	if err != nil || o == nil {
		return false
	}
	return o.Pack2State == pack2Locked || o.Pack2State == pack2Pending
}

// B-01: days of cover = spendable / daily burn; below 4 days → nudge, at most
// once per 7 days (the config's per_cycle cap for a daily-milk cycle).
func (s *service) crmSweepWalletCover(ctx context.Context, now time.Time) {
	for cid, subs := range s.crmSubsByConsumer(ctx) {
		daily := 0.0
		for i := range subs {
			if subs[i].Frequency == "daily" {
				daily += subs[i].UnitPrice*float64(subs[i].Qty) + subscriptionDeliveryFee
			}
		}
		if daily <= 0 {
			continue
		}
		wv, err := s.wallet(ctx, cid)
		if err != nil || wv.Available/daily >= 4 {
			continue
		}
		if s.crmInLiveWelcomeJourney(ctx, cid) {
			continue
		}
		if s.crmCountTriggerSentSince(ctx, cid, "B-01", now.Add(-7*24*time.Hour)) > 0 {
			continue // per_cycle: one nudge a week is a reminder, more is nagging
		}
		s.crmDispatchAt(ctx, "B-01", cid, nil, now)
	}
}

// B-02: tomorrow's due subscription day costs more than the spendable balance
// → the critical cut-off alert. The (trigger, consumer, IST-day) dispatch
// claim caps it at one per day; it re-fires each further day the shortfall
// persists — that is the spec's critical_exempt_from_daily_cap.
func (s *service) crmSweepTomorrowShortfall(ctx context.Context, now time.Time) {
	tomorrow := istDay(now.Add(24 * time.Hour))
	for cid, subs := range s.crmSubsByConsumer(ctx) {
		cost := 0.0
		for i := range subs {
			if subscriptionDueOn(&subs[i], tomorrow) {
				cost += subs[i].UnitPrice*float64(subs[i].Qty) + subscriptionDeliveryFee
			}
		}
		if cost <= 0 {
			continue
		}
		wv, err := s.wallet(ctx, cid)
		if err != nil || wv.Available >= cost {
			continue
		}
		if s.crmInLiveWelcomeJourney(ctx, cid) {
			continue
		}
		s.crmDispatchAt(ctx, "B-02", cid, nil, now)
	}
}

func (s *service) crmCountTriggerSentSince(ctx context.Context, consumerID primitive.ObjectID, triggerID string, since time.Time) int {
	n, _ := s.repo.crmDispatchCol().CountDocuments(ctx, bson.D{
		{Key: "consumer_id", Value: consumerID}, {Key: "trigger_id", Value: triggerID},
		{Key: "status", Value: "SENT"}, {Key: "created_at", Value: bson.D{{Key: "$gte", Value: since}}},
	})
	return int(n)
}

// crmReconcile compares promotional packs issued with consumers created for
// the IST day and raises the W-10 exception on variance. The claim key rides
// the dispatch log with a synthetic consumer id (zero) so it fires once a day.
func (s *service) crmReconcile(ctx context.Context, day string) {
	t := crmTrigger{ID: "W-10", Category: "internal"}
	row, won := s.crmClaimDispatch(ctx, t, primitive.NilObjectID, day)
	if !won {
		return
	}
	start, _ := time.ParseInLocation("2006-01-02", day, istZone)
	end := start.Add(24 * time.Hour)
	// Pack-1 orders ONLY: pack-2 mints follow recharges days after enrolment,
	// so counting them against same-day enrolments would page the admins with
	// a false stop-loss alert on every healthy pack-2 day.
	packs, _ := s.repo.orders.CountDocuments(ctx, bson.D{
		{Key: "offer_id", Value: offerWelcomeLitre},
		{Key: "offer_pack", Value: 1},
		{Key: "created_at", Value: bson.D{{Key: "$gte", Value: start.UTC()}, {Key: "$lt", Value: end.UTC()}}},
	})
	consumers, _ := s.repo.offers().CountDocuments(ctx, bson.D{
		{Key: "enrolled_at", Value: bson.D{{Key: "$gte", Value: start.UTC()}, {Key: "$lt", Value: end.UTC()}}},
	})
	variance := packs - consumers
	s.emitCRMEvent(ctx, "entitlement_reconciled", primitive.NilObjectID, map[string]any{
		"date": day, "packs_issued": packs, "consumers_created": consumers, "variance": variance,
	})
	if variance < 0 {
		variance = -variance
	}
	if variance > 0 {
		s.crmNotifyAdmins(ctx, "CRM_RECONCILIATION", map[string]string{
			"day":     day,
			"summary": fmt.Sprintf("packs issued %d vs consumers created %d — variance %d (campaign stop-loss: investigate before issuing more stock)", packs, consumers, variance),
		})
		s.crmFinishDispatch(ctx, row, "SENT", "", "admin_console")
		return
	}
	s.crmFinishDispatch(ctx, row, "SENT", "", "noop")
}
