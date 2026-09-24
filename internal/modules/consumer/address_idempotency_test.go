package consumer

// F18: the consumer app queues an address it could not send (offline, before
// the session read GET /addresses) and replays it later with POST /addresses.
// When the server already holds that address the replay made a duplicate. A
// create that describes an address the member already has - same label
// (trimmed, case-insensitive), same structured door, and a pin within 5 m or,
// with no pin on either, the same text - returns the stored row with the
// same 201 and the same shape the app reads (lib/api.ts addressFromRemote).
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run AddressCreate -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddressCreateReplayReturnsTheStoredRow(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	cid := w.customer(t, "9000008501", 0)
	clearAddresses(t, w, cid)
	h := &handler{svc: w.svc}

	post := func(body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/addresses", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
		rec := httptest.NewRecorder()
		h.createAddress(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return rec.Code, out
	}
	count := func() int {
		t.Helper()
		list, err := w.svc.repo.listAddresses(context.Background(), cid)
		if err != nil {
			t.Fatalf("listAddresses: %v", err)
		}
		return len(list)
	}

	code, first := post(`{"label":"Home","line1":"P4-805, Chandra Panorama","city":"Lucknow","pincode":"226030","lat":26.771200,"lng":81.012300,"receiver_name":"Asha","ring_bell":true}`)
	if code != http.StatusCreated || first["id"] == "" {
		t.Fatalf("first create: %d %v", code, first)
	}
	// The replay: label spelled differently, the pin re-dropped ~1 m away,
	// the reverse-geocoded text worded differently.
	code, again := post(`{"label":"  home ","line1":"P4 805 Chandra Panorama","city":"Lucknow","pincode":"226030","lat":26.771208,"lng":81.012304}`)
	if code != http.StatusCreated {
		t.Fatalf("replay status %d, want the same 201", code)
	}
	if again["id"] != first["id"] || again["line1"] != first["line1"] || again["receiver_name"] != "Asha" || again["is_default"] != first["is_default"] {
		t.Fatalf("replay must return the stored row unchanged:\nfirst %v\nagain %v", first, again)
	}
	for k := range first {
		if _, ok := again[k]; !ok {
			t.Fatalf("replay response lost key %q", k)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("replay duplicated the address: %d rows", n)
	}

	// Genuinely different addresses still create.
	for _, body := range []string{
		// 50 m away.
		`{"label":"Home","line1":"Somewhere else","city":"Lucknow","pincode":"226030","lat":26.771650,"lng":81.012300}`,
		// Another label at the same pin.
		`{"label":"Office","line1":"P4-805, Chandra Panorama","city":"Lucknow","pincode":"226030","lat":26.771200,"lng":81.012300}`,
		// A structured door the first row does not have.
		`{"label":"Home","line1":"P4-805","city":"Lucknow","pincode":"226030","lat":26.771200,"lng":81.012300,"society":"Chandra Panorama","society_id":"chandra-panorama","tower":"P4","floor":8,"unit":"805"}`,
		// No pin, new text.
		`{"label":"Home","line1":"12, Some Lane","city":"Lucknow","pincode":"226030"}`,
	} {
		before := count()
		if code, _ := post(body); code != http.StatusCreated {
			t.Fatalf("create %s: %d", body, code)
		}
		if count() != before+1 {
			t.Fatalf("a different address was folded into an existing one: %s", body)
		}
	}
	// With no pin on either side, identical text is the same address.
	before := count()
	if code, _ := post(`{"label":"HOME","line1":" 12, Some Lane ","city":"lucknow","pincode":"226030"}`); code != http.StatusCreated {
		t.Fatalf("pinless replay: %d", code)
	}
	if count() != before {
		t.Fatalf("a pinless replay with the same text duplicated the address")
	}
	// Two flats of one tower share a pin: a different unit is a different door.
	before = count()
	if code, _ := post(`{"label":"Home","line1":"P4-806","city":"Lucknow","pincode":"226030","lat":26.771200,"lng":81.012300,"society":"Chandra Panorama","society_id":"chandra-panorama","tower":"P4","floor":8,"unit":"806"}`); code != http.StatusCreated {
		t.Fatal("second flat")
	}
	if count() != before+1 {
		t.Fatal("a different flat at the same pin was folded into the first")
	}
}

// R1-07: the app always sends label "Home", and outside a society the flat
// lives only in line1. Pins within 5 m used to be enough, so a second flat in
// the same building (or a member correcting their flat number) came back as
// the OLD row and the new door was silently discarded.
func TestAddressCreateDifferentFlatAtTheSamePinIsKept(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	cid := w.customer(t, "9000008502", 0)
	clearAddresses(t, w, cid)
	lat, lng := 26.7800, 81.0200
	mk := func(line1 string, lt float64) *address {
		t.Helper()
		ad, err := w.svc.createAddress(ctx, cid, addressInput{Label: "Home", Line1: line1, Line2: "Gomti Nagar", City: "Lucknow", Pincode: "226010", IsDefault: true, Lat: &lt, Lng: &lng})
		if err != nil {
			t.Fatalf("createAddress %q: %v", line1, err)
		}
		return ad
	}
	first := mk("Flat 3, Shanti Apartments", lat)
	second := mk("Flat 7, Shanti Apartments", lat+0.00002) // ~2 m away
	if second.ID == first.ID || second.Line1 != "Flat 7, Shanti Apartments" {
		t.Fatalf("a different flat was folded into the first: got %s %q, first %s", second.ID.Hex(), second.Line1, first.ID.Hex())
	}
	list, err := w.svc.listAddresses(ctx, cid)
	if err != nil || len(list) != 2 {
		t.Fatalf("addresses after two flats: %d %v", len(list), err)
	}
	// The genuine replay (same text, punctuation aside, pin re-dropped) still
	// returns the stored row.
	again := mk("flat 7 shanti apartments", lat+0.00001)
	if again.ID != second.ID {
		t.Fatalf("a replay of flat 7 created a new row: %s want %s", again.ID.Hex(), second.ID.Hex())
	}
}
