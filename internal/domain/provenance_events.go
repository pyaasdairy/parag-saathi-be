package domain

// Provenance event types — the append-only vocabulary of the pour→QR chain
// (blueprint §7). Corrections are NEW events referencing the superseded one;
// nothing is ever edited in place.
const (
	// Collection
	EventPourRecorded    = "pour.recorded"
	EventPourSuperseded  = "pour.superseded"
	EventReadingRecorded = "reading.recorded"
	EventInvoiceIssued   = "invoice.issued"
	// EventInvoiceAmended records late pours merged into an already-issued
	// invoice (e.g. the evening shift generated after the morning run).
	EventInvoiceAmended = "invoice.amended"

	// Logistics
	EventConsignmentCreated    = "consignment.created"
	EventConsignmentDispatched = "consignment.dispatched"
	EventConsignmentPickedUp   = "consignment.picked_up"
	EventTripDelivered         = "trip.delivered"
	EventColdChainLogged       = "trip.cold_chain_logged"

	// Per-samiti batch flow (F6/F7): plant intake verdicts, batch QC and the
	// auto-minted batch QR.
	EventConsignmentPlantAccepted = "consignment.plant_accepted"
	EventConsignmentPlantRejected = "consignment.plant_rejected"
	EventConsignmentQCRecorded    = "consignment.qc_recorded"
	EventConsignmentQRIssued      = "consignment.qr_issued"

	// BMC / plant
	EventBMCLotCreated    = "bmc_lot.created"
	EventBMCLotClosed     = "bmc_lot.closed"
	EventBMCLotDispatched = "bmc_lot.dispatched"
	EventBatchCreated     = "batch.created"
	EventBatchCompleted   = "batch.completed"
	EventProductLotMade   = "product_lot.created"
	EventProductRecalled  = "product_lot.recalled"
	EventQRIssued         = "qr.issued"

	// Quality / safety gate
	EventQCRecorded  = "qc.recorded"
	EventGatePassed  = "qc.gate_passed"
	EventGateBlocked = "qc.gate_blocked"
	// HOLD lifecycle (§13.5): a subject quarantined pending resolution, and the
	// analyst's later HOLD→PASS/REJECT resolution.
	EventGateHold     = "qc.gate_hold"
	EventGateResolved = "qc.gate_resolved"

	// Settlement
	EventSettlementInitiated = "settlement.initiated"
	EventSettlementApproved  = "settlement.approved"
	EventSettlementExecuted  = "settlement.executed"
	EventPayoutCredited      = "payout.credited"

	// Cattle
	EventAnimalRegistered  = "animal.registered"
	EventHealthEventLogged = "health.event_logged"
)

// Provenance entity types (the nodes of the trace graph).
const (
	EntityMilkPour        = "MILK_POUR"
	EntityAnalyzerReading = "ANALYZER_READING"
	EntityInvoice         = "INVOICE"
	EntityConsignment     = "DCS_CONSIGNMENT"
	EntityRouteTrip       = "ROUTE_TRIP"
	EntityBMCLot          = "BMC_LOT"
	EntityBatch           = "PROCESSING_BATCH"
	EntityProductLot      = "PRODUCT_LOT"
	EntityBatchQR         = "BATCH_QR"
	// Per-samiti batch flow: the batch QC record and its public QR.
	EntityConsignmentQC      = "CONSIGNMENT_QC"
	EntityConsignmentBatchQR = "CONSIGNMENT_BATCH_QR"
	EntityQCResult           = "QC_RESULT"
	EntitySettlement         = "SETTLEMENT_BATCH"
	EntityAnimal             = "ANIMAL"
	EntityParty              = "PARTY"
)
