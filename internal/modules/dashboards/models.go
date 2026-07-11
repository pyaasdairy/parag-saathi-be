package dashboards

// FarmerSummary is the farmer home-screen aggregate: today, this month, what's
// pending, and a short daily trend — computed server-side to cut round-trips.
type FarmerSummary struct {
	FarmerPartyID   string       `json:"farmer_party_id"`
	Today           DayTotals    `json:"today"`
	Month           PeriodTotals `json:"month"`
	PendingAmount   float64      `json:"pending_amount"`   // issued-but-unpaid invoice total
	PendingInvoices int          `json:"pending_invoices"` // count of unpaid invoices
	AnimalCount     int          `json:"animal_count"`     // registered animals owned by this farmer
	Trend           []DayTotals  `json:"trend"`            // most-recent days first
	// Sachiv is the farmer's assigned society secretary (F2): the newest ACTIVE
	// SAMITI_SACHEEV at the farmer's own DCS. Omitted when none resolves —
	// never fabricated.
	Sachiv *SachivInfo `json:"sachiv,omitempty"`
}

// SachivInfo names the farmer's assigned Sachiv with contact + society info.
type SachivInfo struct {
	PartyID string `json:"party_id"`
	Name    string `json:"name"`
	NameHi  string `json:"name_hi,omitempty"`
	Phone   string `json:"phone"`
	Village string `json:"village,omitempty"`
	DCSName string `json:"dcs_name,omitempty"`
}

// SocietyStats is the DCS console aggregate for the sachiv/adhyaksh.
type SocietyStats struct {
	DCSID              string       `json:"dcs_id"`
	Date               string       `json:"date"`
	Today              DayTotals    `json:"today"`
	Month              PeriodTotals `json:"month"`
	ActiveFarmers      int          `json:"active_farmers"`       // distinct farmers who poured today
	MemberCount        int          `json:"member_count"`         // active FARMER assignments at this DCS
	AvgFatPct          float64      `json:"avg_fat_pct"`          // month quantity-weighted average fat
	AvgSNFPct          float64      `json:"avg_snf_pct"`          // month quantity-weighted average SNF
	QualityFailures30d int          `json:"quality_failures_30d"` // this DCS's consignments rejected in 30d
	OpenConsignment    bool         `json:"open_consignment"`     // is today's shift still unsealed
	Trend              []DayTotals  `json:"trend"`                // most-recent days first
}

// DayTotals is one day's pour rollup.
type DayTotals struct {
	Date           string  `json:"date"`
	QuantityLitres float64 `json:"quantity_litres"`
	Amount         float64 `json:"amount"`
	Pours          int     `json:"pours"`
}

// PeriodTotals is a multi-day rollup.
type PeriodTotals struct {
	QuantityLitres float64 `json:"quantity_litres"`
	Amount         float64 `json:"amount"`
	Pours          int     `json:"pours"`
}
