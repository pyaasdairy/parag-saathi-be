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

	// BMC / plant
	EventBMCLotCreated    = "bmc_lot.created"
	EventBMCLotClosed     = "bmc_lot.closed"
	EventBMCLotDispatched = "bmc_lot.dispatched"
	EventBatchCreated    = "batch.created"
	EventBatchCompleted  = "batch.completed"
	EventProductLotMade  = "product_lot.created"
	EventProductRecalled = "product_lot.recalled"
	EventQRIssued        = "qr.issued"

	// Quality / safety gate
	EventQCRecorded  = "qc.recorded"
	EventGatePassed  = "qc.gate_passed"
	EventGateBlocked = "qc.gate_blocked"

	// Settlement
	EventSettlementInitiated = "settlement.initiated"
	EventSettlementApproved  = "settlement.approved"
	EventSettlementExecuted  = "settlement.executed"
	EventPayoutCredited      = "payout.credited"

	// Cattle
	EventAnimalRegistered   = "animal.registered"
	EventHealthEventLogged  = "health.event_logged"
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
	EntityQCResult        = "QC_RESULT"
	EntitySettlement      = "SETTLEMENT_BATCH"
	EntityAnimal          = "ANIMAL"
	EntityParty           = "PARTY"
)
