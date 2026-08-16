// Package dolibarr is a minimal, read-mostly REST client for the Dolibarr ERP
// (DoliCloud, v23.x). It exists for exactly two integration lanes (see
// docs/Pyaas_Dolibarr_WriteAPI_Enablement_Brief):
//
//   - INBOUND  (catalog sync): list sellable products so the consumer catalog can
//     mirror the ERP product master — Dolibarr owns product/price, Saathi mirrors.
//   - OUTBOUND (stock ledger): post the daily NET stock-out movement per SKU —
//     the ONLY write this client performs. Stock-IN / GRN is booked by a human in
//     the Dolibarr UI, never by the API (one writer per direction).
//
// Hard rules encoded here rather than trusted to callers:
//   - Idempotency: stock movements are an append-only ledger and Dolibarr does
//     NOT dedupe them, so PostStockOut REFUSES to post unless the caller first
//     proves the movementcode is unused (MovementExists → query-before-post).
//   - Tolerant JSON: the production instance intermittently prefixes every HTTP
//     response with "Failed to write to log file dolibarr.log" noise; decode
//     starts at the first '{' or '[' so a misconfigured syslog can't break sync.
//   - Empty lists: Dolibarr list endpoints return HTTP 404 when there are zero
//     rows — that is "empty", not an error.
package dolibarr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Dolibarr instance with one API key. Zero-value disabled;
// construct with New. Safe for concurrent use.
type Client struct {
	base string // e.g. https://pyaas.with10.dolicloud.com/api/index.php
	key  string // DOLAPIKEY of the scoped integration user (saathi.sync)
	hc   *http.Client
}

// New returns a client, or nil when base/key are unset (integration off).
func New(base, key string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	key = strings.TrimSpace(key)
	if base == "" || key == "" {
		return nil
	}
	return &Client{base: base, key: key, hc: &http.Client{Timeout: 20 * time.Second}}
}

// Num tolerates Dolibarr's mixed JSON numerics: "35.00000000", 35, 35.0, null,
// "" all decode; access via Float/Int.
type Num struct{ raw string }

func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, `"`)
	if s == "null" {
		s = ""
	}
	n.raw = s
	return nil
}
func (n Num) Float() float64 { f, _ := strconv.ParseFloat(n.raw, 64); return f }
func (n Num) Int() int64     { f, _ := strconv.ParseFloat(n.raw, 64); return int64(f) }

// Product is the slice of Dolibarr's product object the sync consumes.
// (Field semantics per the 23.0.3 explorer; do not add fields blindly —
// every field here is mapped somewhere in dolibarr_sync.go.)
type Product struct {
	ID          Num    `json:"id"`
	Ref         string `json:"ref"`         // stable business key → catalogDoc.base_id
	Label       string `json:"label"`       // product name
	Description string `json:"description"` //
	PriceTTC    Num    `json:"price_ttc"`   // selling price incl. tax
	PriceMin    Num    `json:"price_min"`   // MRP per current ERP convention
	Barcode     string `json:"barcode"`     //
	Volume      Num    `json:"volume"`      // with volume_units -6 ⇒ millilitres
	VolumeUnits Num    `json:"volume_units"`
	Weight      Num    `json:"weight"`
	ToSell      Num    `json:"status"`     // 1 = on sale
	Finished    Num    `json:"finished"`   // 1 = finished good
	StockReel   Num    `json:"stock_reel"` // on-hand (with includestockdata=1)
	// Tax fields: this instance stores prices ex-GST (price_base_type=HT) with
	// tva_tx often 0 and the real rate only in the vat code ("C+S-5" = 5%).
	TvaTx          Num    `json:"tva_tx"`
	DefaultVATCode string `json:"default_vat_code"`
}

// Sellable reports whether the consumer catalog should surface this product.
func (p Product) Sellable() bool { return p.ToSell.Int() == 1 }

// VolumeMl normalises Dolibarr's (volume, volume_units) to millilitres; 0 when
// unknown. units==-6 is ml; units==0 with a small number is treated as litres.
func (p Product) VolumeMl() float64 {
	v := p.Volume.Float()
	if v == 0 {
		return 0
	}
	switch p.VolumeUnits.Int() {
	case -6:
		return v
	case -3: // dm³ = litres
		return v * 1000
	default:
		return v // instance convention: raw value is already ml
	}
}

// ListSellableProducts pulls every on-sale product (paged; includes stock).
func (c *Client) ListSellableProducts(ctx context.Context) ([]Product, error) {
	var all []Product
	for page := 0; ; page++ {
		q := url.Values{
			"mode":             {"1"}, // on-sale only
			"includestockdata": {"1"},
			"limit":            {"100"},
			"page":             {strconv.Itoa(page)},
			"sortfield":        {"t.ref"},
			"sortorder":        {"ASC"},
		}
		var batch []Product
		found, err := c.getJSON(ctx, "/products?"+q.Encode(), &batch)
		if err != nil {
			return nil, err
		}
		if !found {
			break // 404 = no (more) rows
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// MovementExists reports whether a stock movement with this movementcode was
// already posted (the idempotency probe; movementcode lands in inventorycode).
func (c *Client) MovementExists(ctx context.Context, code string) (bool, error) {
	q := url.Values{
		"limit":      {"1"},
		"sqlfilters": {fmt.Sprintf("(t.inventorycode:=:'%s')", strings.ReplaceAll(code, "'", ""))},
	}
	var rows []json.RawMessage
	found, err := c.getJSON(ctx, "/stockmovements?"+q.Encode(), &rows)
	if err != nil {
		return false, err
	}
	return found && len(rows) > 0, nil
}

// StockOut is one daily NET consumer-delivery movement for one SKU.
type StockOut struct {
	ProductID   int64  // Dolibarr product id
	WarehouseID int64  // 2 = MOBILE HUB (id 1 AT PLANT is UI-only)
	Qty         int64  // POSITIVE net units delivered; posted as -Qty (output)
	Code        string // movementcode, e.g. SAATHI-STKOUT-2026-08-15-Parag_Gold
	Label       string // human label, e.g. "Daily net consumer delivery 2026-08-15"
	Datem       string // "YYYY-MM-DD HH:MM:SS" movement datetime
}

// PostStockOut books m as an OUTPUT movement — after re-checking the
// movementcode is unused, so a retry/replay can never double-decrement.
// Returns (posted=false) without error when the movement already exists.
func (c *Client) PostStockOut(ctx context.Context, m StockOut) (posted bool, err error) {
	if m.ProductID <= 0 || m.WarehouseID <= 0 || m.Qty <= 0 || m.Code == "" {
		return false, fmt.Errorf("dolibarr: invalid stock-out %+v", m)
	}
	exists, err := c.MovementExists(ctx, m.Code)
	if err != nil {
		return false, fmt.Errorf("dolibarr: idempotency probe %s: %w", m.Code, err)
	}
	if exists {
		return false, nil
	}
	body := map[string]any{
		"product_id":   m.ProductID,
		"warehouse_id": m.WarehouseID,
		"qty":          -m.Qty, // negative = stock out
		"type":         1,      // 1 = output movement
		"movementcode": m.Code,
		// live-verified on 23.0.3: the ledger row's label comes from "label";
		// "movementlabel" is accepted but not persisted — send both.
		"label":         m.Label,
		"movementlabel": m.Label,
	}
	if m.Datem != "" {
		body["datem"] = m.Datem
	}
	if err := c.postJSON(ctx, "/stockmovements", body); err != nil {
		return false, err
	}
	return true, nil
}

// ── product documents (images) ───────────────────────────────────────────────

// Document is one file attached to a product in Dolibarr's ECM.
type Document struct {
	// Level1Name is the storage folder — the product ref as stored (uppercased
	// by Dolibarr), which download paths must use.
	Level1Name string `json:"level1name"`
	// RelativeName is the bare filename inside that folder.
	RelativeName string `json:"relativename"`
	Size         Num    `json:"size"`
}

// IsImage reports whether the attachment looks like a product photo.
func (d Document) IsImage() bool {
	n := strings.ToLower(d.RelativeName)
	return strings.HasSuffix(n, ".jpg") || strings.HasSuffix(n, ".jpeg") ||
		strings.HasSuffix(n, ".png") || strings.HasSuffix(n, ".webp")
}

// ListProductDocuments lists files attached to the product with this ref.
// Empty (not error) when the product has no attachments.
func (c *Client) ListProductDocuments(ctx context.Context, ref string) ([]Document, error) {
	q := url.Values{"modulepart": {"product"}, "ref": {ref}}
	var docs []Document
	found, err := c.getJSON(ctx, "/documents?"+q.Encode(), &docs)
	if err != nil || !found {
		return nil, err // 404 = no documents (or product unknown) → empty
	}
	return docs, nil
}

// DownloadProductDoc fetches one attached file's bytes (Dolibarr returns them
// base64-wrapped). folder is Document.Level1Name; file is Document.RelativeName.
func (c *Client) DownloadProductDoc(ctx context.Context, folder, file string) ([]byte, error) {
	q := url.Values{
		"modulepart":    {"product"},
		"original_file": {folder + "/" + file},
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	found, err := c.getJSON(ctx, "/documents/download?"+q.Encode(), &out)
	if err != nil {
		return nil, err
	}
	if !found || out.Content == "" {
		return nil, fmt.Errorf("dolibarr: document %s/%s not found", folder, file)
	}
	raw, err := base64.StdEncoding.DecodeString(out.Content)
	if err != nil {
		return nil, fmt.Errorf("dolibarr: document %s/%s decode: %w", folder, file, err)
	}
	return raw, nil
}

// ── transport ────────────────────────────────────────────────────────────────

// getJSON GETs path and decodes into out. found=false (no error) on HTTP 404,
// which Dolibarr list endpoints use for "zero rows".
func (c *Client) getJSON(ctx context.Context, path string, out any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("DOLAPIKEY", c.key)
	res, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return false, err
	}
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if res.StatusCode >= 400 {
		return false, fmt.Errorf("dolibarr: GET %s → %d: %s", path, res.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(cleanJSON(raw), out); err != nil {
		return false, fmt.Errorf("dolibarr: GET %s decode: %w (%s)", path, err, snippet(raw))
	}
	return true, nil
}

func (c *Client) postJSON(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("DOLAPIKEY", c.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 400 {
		return fmt.Errorf("dolibarr: POST %s → %d: %s", path, res.StatusCode, snippet(raw))
	}
	return nil
}

// cleanJSON drops any garbage before the first JSON token — the production
// instance has been observed prefixing responses with syslog-failure lines
// ("Failed to write to log file dolibarr.log"), which must not break the sync.
func cleanJSON(raw []byte) []byte {
	if i := bytes.IndexAny(raw, "{["); i > 0 {
		return raw[i:]
	}
	return raw
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
