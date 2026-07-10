package dashboards

// FarmerSummary is the farmer home-screen aggregate: today, this month, what's
// pending, and a short daily trend — computed server-side to cut round-trips.
type FarmerSummary struct {
	FarmerPartyID   string       `json:"farmer_party_id"`
	Today           DayTotals    `json:"today"`
	Month           PeriodTotals `json:"month"`
	PendingAmount   float64      `json:"pending_amount"`   // issued-but-unpaid invoice total
	PendingInvoices int          `json:"pending_invoices"` // count of unpaid invoices
	AnimalCount     int          `json:"animal_count"`     // farmer's ACTIVE animals
	Trend           []DayTotals  `json:"trend"`            // most-recent days first
}

// SocietyStats is the DCS console aggregate for the sachiv/adhyaksh.
type SocietyStats struct {
	DCSID           string       `json:"dcs_id"`
	Date            string       `json:"date"`
	Today           DayTotals    `json:"today"`
	Month           PeriodTotals `json:"month"`
	ActiveFarmers   int          `json:"active_farmers"`   // distinct farmers who poured today
	MemberCount     int          `json:"member_count"`     // active FARMER assignments at this DCS
	OpenConsignment bool         `json:"open_consignment"` // is today's shift still unsealed
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
