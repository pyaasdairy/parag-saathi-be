package httpx

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ParseID converts an ObjectID hex string (from JSON bodies, query params or
// JWT claims) into a primitive.ObjectID, or returns a 400 with the offending
// field name.
func ParseID(hex, field string) (primitive.ObjectID, error) {
	id, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		return primitive.NilObjectID, BadRequest("INVALID_ID", field+" must be a valid object id")
	}
	return id, nil
}

// PathID extracts a chi URL parameter and parses it as an ObjectID.
func PathID(r *http.Request, param string) (primitive.ObjectID, error) {
	return ParseID(chi.URLParam(r, param), param)
}
