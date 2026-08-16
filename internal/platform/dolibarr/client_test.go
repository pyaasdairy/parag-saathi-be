package dolibarr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockERP is a fake Dolibarr — tests NEVER touch the production instance.
func mockERP(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-key")
}

func TestNewDisabledWhenUnconfigured(t *testing.T) {
	if New("", "key") != nil || New("http://x", "") != nil || New("", "") != nil {
		t.Fatal("client must be nil when base or key is missing")
	}
}

func TestListProductsTolleratesLogPollutionAndPaginates(t *testing.T) {
	page := 0
	c := mockERP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DOLAPIKEY") != "test-key" {
			w.WriteHeader(401)
			return
		}
		// page 0 → 100 rows (forces a second page), page 1 → 404 (Dolibarr's "no rows")
		if r.URL.Query().Get("page") == "0" {
			rows := make([]map[string]any, 100)
			for i := range rows {
				rows[i] = map[string]any{"id": i + 1, "ref": fmt.Sprintf("PRG-X-%d", i), "label": "X",
					"price_ttc": "10.00000000", "status": "1"}
			}
			b, _ := json.Marshal(rows)
			// the production instance prefixes responses with syslog noise —
			// the client must decode anyway
			w.Write([]byte("Failed to write to log file dolibarr.log\nFailed to write to log file dolibarr.log\n"))
			w.Write(b)
			return
		}
		page = 1
		http.Error(w, `{"error":{"code":404,"message":"Not Found"}}`, 404)
	})
	got, err := c.ListSellableProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 || page != 1 {
		t.Fatalf("want 100 products across 2 pages, got %d (page2 hit=%v)", len(got), page == 1)
	}
	if got[0].PriceTTC.Float() != 10 || got[0].ToSell.Int() != 1 {
		t.Fatalf("mixed-type numerics not decoded: %+v", got[0])
	}
}

func TestMovementExists(t *testing.T) {
	c := mockERP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sqlfilters") == "(t.inventorycode:=:'SAATHI-STKOUT-2026-08-15-PRG-FCM-500ML')" {
			w.Write([]byte(`[{"id":"9"}]`))
			return
		}
		http.Error(w, `{"error":{"code":404}}`, 404) // zero rows
	})
	if ok, err := c.MovementExists(context.Background(), "SAATHI-STKOUT-2026-08-15-PRG-FCM-500ML"); err != nil || !ok {
		t.Fatalf("existing code: ok=%v err=%v", ok, err)
	}
	if ok, err := c.MovementExists(context.Background(), "SAATHI-STKOUT-2026-08-16-PRG-FCM-500ML"); err != nil || ok {
		t.Fatalf("fresh code must not exist: ok=%v err=%v", ok, err)
	}
}

func TestPostStockOutIsIdempotentAndOutputTyped(t *testing.T) {
	var posted []map[string]any
	c := mockERP(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET": // idempotency probe
			if len(posted) > 0 {
				w.Write([]byte(`[{"id":"1"}]`))
			} else {
				http.Error(w, `{}`, 404)
			}
		case r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			posted = append(posted, body)
			w.Write([]byte(`7`))
		}
	})
	m := StockOut{ProductID: 128, WarehouseID: 2, Qty: 1178,
		Code: "SAATHI-STKOUT-2026-08-15-PRG-FCM-500ML", Label: "Daily net consumer delivery 2026-08-15"}

	ok, err := c.PostStockOut(context.Background(), m)
	if err != nil || !ok {
		t.Fatalf("first post: ok=%v err=%v", ok, err)
	}
	b := posted[0]
	if b["qty"].(float64) != -1178 || b["type"].(float64) != 1 || b["warehouse_id"].(float64) != 2 {
		t.Fatalf("movement must be a negative OUTPUT to wh2: %+v", b)
	}
	// replay: the probe now finds the code → NO second post (ledger never double-decrements)
	ok, err = c.PostStockOut(context.Background(), m)
	if err != nil || ok || len(posted) != 1 {
		t.Fatalf("replay must skip: ok=%v err=%v posts=%d", ok, err, len(posted))
	}
}

func TestPostStockOutRefusesInvalid(t *testing.T) {
	c := mockERP(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("must not reach the ERP") })
	for _, m := range []StockOut{
		{},
		{ProductID: 1, WarehouseID: 2, Qty: 0, Code: "X"},  // zero qty
		{ProductID: 1, WarehouseID: 2, Qty: -5, Code: "X"}, // caller passes NET positive
		{ProductID: 1, WarehouseID: 2, Qty: 5},             // no idempotency code
	} {
		if _, err := c.PostStockOut(context.Background(), m); err == nil {
			t.Fatalf("invalid movement accepted: %+v", m)
		}
	}
}

func TestDocumentsListAndDownload(t *testing.T) {
	img := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3} // JPEG magic
	c := mockERP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/documents":
			w.Write([]byte(`[{"level1name":"PRG-TONED-500ML","relativename":"pack (1).jpeg","size":"81850"},
			                 {"level1name":"PRG-TONED-500ML","relativename":"specs.pdf","size":"100"}]`))
		case "/documents/download":
			if r.URL.Query().Get("original_file") != "PRG-TONED-500ML/pack (1).jpeg" {
				http.Error(w, `{}`, 404)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"filename": "pack (1).jpeg", "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString(img)})
		}
	})
	docs, err := c.ListProductDocuments(context.Background(), "prg-toned-500ml")
	if err != nil || len(docs) != 2 {
		t.Fatalf("docs: %v %v", docs, err)
	}
	if !docs[0].IsImage() || docs[1].IsImage() {
		t.Fatal("image detection wrong")
	}
	raw, err := c.DownloadProductDoc(context.Background(), docs[0].Level1Name, docs[0].RelativeName)
	if err != nil || string(raw) != string(img) {
		t.Fatalf("download: %v len=%d", err, len(raw))
	}
}

func TestVolumeMl(t *testing.T) {
	mk := func(v, units string) Product {
		var p Product
		json.Unmarshal([]byte(`{"volume":"`+v+`","volume_units":"`+units+`"}`), &p)
		return p
	}
	if got := mk("500", "-6").VolumeMl(); got != 500 {
		t.Fatalf("ml: %v", got)
	}
	if got := mk("1", "-3").VolumeMl(); got != 1000 {
		t.Fatalf("litres: %v", got)
	}
	if got := mk("", "").VolumeMl(); got != 0 {
		t.Fatalf("empty: %v", got)
	}
}
