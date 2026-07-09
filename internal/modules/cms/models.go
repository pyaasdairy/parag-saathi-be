package cms

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CreateContentRequest is the body of POST /content. RegionRef arrives as a
// plain ObjectID hex string in JSON and unmarshals natively; nil means the
// item is not pinned to a specific org unit.
type CreateContentRequest struct {
	Type            string              `json:"type"`
	TitleI18n       map[string]string   `json:"title_i18n"`
	DescriptionI18n map[string]string   `json:"description_i18n,omitempty"`
	URL             string              `json:"url,omitempty"`
	ThumbnailURL    string              `json:"thumbnail_url,omitempty"`
	PhoneNumbers    []string            `json:"phone_numbers,omitempty"`
	Languages       []string            `json:"languages,omitempty"`
	RegionScope     string              `json:"region_scope,omitempty"`
	RegionRef       *primitive.ObjectID `json:"region_ref,omitempty"`
	Category        string              `json:"category,omitempty"`
	Order           int                 `json:"order,omitempty"`
	Published       bool                `json:"published,omitempty"`
	ValidFrom       *time.Time          `json:"valid_from,omitempty"`
	ValidTo         *time.Time          `json:"valid_to,omitempty"`
}

// UpdateContentRequest is the body of PUT /content/{id}. Pointer fields
// distinguish "absent" from "set to zero value"; any applied edit mints a
// fresh Version so the delta pull carries the change to the field app.
type UpdateContentRequest struct {
	Type            *string             `json:"type"`
	TitleI18n       *map[string]string  `json:"title_i18n"`
	DescriptionI18n *map[string]string  `json:"description_i18n"`
	URL             *string             `json:"url"`
	ThumbnailURL    *string             `json:"thumbnail_url"`
	PhoneNumbers    *[]string           `json:"phone_numbers"`
	Languages       *[]string           `json:"languages"`
	RegionScope     *string             `json:"region_scope"`
	RegionRef       *primitive.ObjectID `json:"region_ref"`
	Category        *string             `json:"category"`
	Order           *int                `json:"order"`
	Published       *bool               `json:"published"`
	ValidFrom       *time.Time          `json:"valid_from"`
	ValidTo         *time.Time          `json:"valid_to"`
}

// DeltaMeta is the meta of GET /content: how many items this page carried,
// the highest version in it (the cursor the client passes back as ?since=),
// and whether more changed items remain beyond the batch cap.
type DeltaMeta struct {
	Count      int   `json:"count"`
	MaxVersion int64 `json:"max_version"`
	Truncated  bool  `json:"truncated"`
}

// HelplineMeta is the meta of GET /content/helpline.
type HelplineMeta struct {
	Count int `json:"count"`
}
